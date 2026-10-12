package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const continuationEntryLimit = 4 << 20

type conversationKey struct{ Key, Model, Path, ID string }
type conversation struct {
	Input   []byte
	AuthID  string
	Expires time.Time
}

// ConversationStore is volatile, bounded, and never persists request content.
// HTTP clients can upload only new input even when the provider needs replay.
// A cache miss is terminal: the client must explicitly resend full context.
type ConversationStore struct {
	mu                          sync.Mutex
	items                       map[conversationKey]conversation
	bytes, maxBytes, maxEntries int
	ttl                         time.Duration
	now                         func() time.Time
}

func NewConversationStore() *ConversationStore {
	return &ConversationStore{items: map[conversationKey]conversation{}, maxBytes: 32 << 20, maxEntries: 128, ttl: 15 * time.Minute, now: time.Now}
}

func (s *ConversationStore) get(key conversationKey) (conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.items[key]
	if ok && !s.now().Before(v.Expires) {
		s.bytes -= len(v.Input)
		delete(s.items, key)
		return conversation{}, false
	}
	return v, ok
}

func (s *ConversationStore) put(key conversationKey, input []byte, authID string) {
	if len(input) > continuationEntryLimit || key.ID == "" || len(key.ID) > 1024 || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, v := range s.items {
		if !now.Before(v.Expires) {
			s.bytes -= len(v.Input)
			delete(s.items, k)
		}
	}
	if old, ok := s.items[key]; ok {
		s.bytes -= len(old.Input)
		delete(s.items, key)
	}
	for len(s.items) >= s.maxEntries || s.bytes+len(input) > s.maxBytes {
		var oldest conversationKey
		var expires time.Time
		for k, v := range s.items {
			if expires.IsZero() || v.Expires.Before(expires) {
				oldest, expires = k, v.Expires
			}
		}
		if expires.IsZero() {
			return
		}
		s.bytes -= len(s.items[oldest].Input)
		delete(s.items, oldest)
	}
	s.items[key] = conversation{bytes.Clone(input), authID, now.Add(s.ttl)}
	s.bytes += len(input)
}

func strictContinuationFields(body []byte) error {
	d := json.NewDecoder(bytes.NewReader(body))
	if _, err := d.Token(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return err
		}
		var v json.RawMessage
		if err = d.Decode(&v); err != nil {
			return err
		}
		name := k.(string)
		if seen[name] {
			return fmt.Errorf("duplicate request field")
		}
		seen[name] = true
	}
	return nil
}

func inputArray(v gjson.Result) ([]json.RawMessage, error) {
	if !v.Exists() || v.Type == gjson.Null {
		return nil, nil
	}
	if v.Type == gjson.String {
		b, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": v.String()})
		return []json.RawMessage{b}, nil
	}
	if !v.IsArray() {
		return nil, fmt.Errorf("input must be a string or array")
	}
	var items []json.RawMessage
	err := json.Unmarshal([]byte(v.Raw), &items)
	return items, err
}

func (s *ConversationStore) prepare(body []byte, key conversationKey, pinned string) ([]byte, string, error) {
	if err := strictContinuationFields(body); err != nil {
		return nil, "", err
	}
	prev := gjson.GetBytes(body, "previous_response_id")
	if !prev.Exists() || prev.Type == gjson.Null {
		return body, pinned, nil
	}
	if prev.Type != gjson.String || prev.String() == "" || len(prev.String()) > 1024 {
		return nil, "", fmt.Errorf("invalid previous_response_id")
	}
	key.ID = prev.String()
	prior, ok := s.get(key)
	if !ok || pinned != "" && prior.AuthID != pinned {
		return nil, "", fmt.Errorf("previous_response_id unavailable; resend full context without previous_response_id")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(prior.Input, &items); err != nil {
		return nil, "", err
	}
	delta, err := inputArray(gjson.GetBytes(body, "input"))
	if err != nil {
		return nil, "", err
	}
	combined, err := json.Marshal(append(items, delta...))
	if err != nil {
		return nil, "", err
	}
	if len(combined) > int(MaxBodyBytes) {
		return nil, "", fmt.Errorf("expanded conversation exceeds 64 MiB")
	}
	body, err = sjson.SetRawBytes(body, "input", combined)
	if err != nil {
		return nil, "", err
	}
	body, err = sjson.DeleteBytes(body, "previous_response_id")
	if len(body) > int(MaxBodyBytes) {
		return nil, "", fmt.Errorf("expanded conversation exceeds 64 MiB")
	}
	return body, prior.AuthID, err
}

// capture observes identity bytes before the downstream compression layer. It
// buffers at most one event, not the whole stream, and never delays Flush.
type continuationWriter struct {
	gin.ResponseWriter
	buffer                    []byte
	response                  []byte
	sse, initialized, invalid bool
}

func (w *continuationWriter) Write(p []byte) (int, error) {
	if !w.initialized {
		w.initialized = true
		w.sse = strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
	}
	if !w.invalid {
		if len(w.buffer)+len(p) > continuationEntryLimit {
			w.invalid = true
			w.buffer = nil
			w.response = nil
		} else {
			w.buffer = append(w.buffer, p...)
			if w.sse {
				w.consume()
			}
		}
	}
	return w.ResponseWriter.Write(p)
}
func (w *continuationWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func (w *continuationWriter) consume() {
	for {
		end, sep := bytes.Index(w.buffer, []byte("\n\n")), 2
		if cr := bytes.Index(w.buffer, []byte("\r\n\r\n")); cr >= 0 && (end < 0 || cr < end) {
			end, sep = cr, 4
		}
		if end < 0 {
			return
		}
		event := w.buffer[:end]
		var data []byte
		for _, line := range bytes.Split(event, []byte("\n")) {
			line = bytes.TrimSuffix(line, []byte("\r"))
			if bytes.HasPrefix(line, []byte("data:")) {
				line = bytes.TrimPrefix(line, []byte("data:"))
				line = bytes.TrimPrefix(line, []byte(" "))
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, line...)
			}
		}
		switch gjson.GetBytes(data, "type").String() {
		case "response.completed":
			w.response = bytes.Clone([]byte(gjson.GetBytes(data, "response").Raw))
		case "error", "response.failed", "response.incomplete":
			w.invalid = true
			w.response = nil
		}
		w.buffer = w.buffer[end+sep:]
	}
}

func (w *continuationWriter) completed() []byte {
	if w.invalid || w.Status() != 200 {
		return nil
	}
	if !w.sse {
		return w.buffer
	}
	if len(bytes.TrimSpace(w.buffer)) != 0 {
		return nil
	}
	return w.response
}

func (s *ConversationStore) save(key conversationKey, body, response []byte, authID string) {
	if !gjson.ValidBytes(response) || gjson.GetBytes(response, "status").String() != "completed" {
		return
	}
	output := gjson.GetBytes(response, "output")
	if !output.IsArray() {
		return
	}
	input, err := inputArray(gjson.GetBytes(body, "input"))
	if err != nil {
		return
	}
	var generated []json.RawMessage
	if json.Unmarshal([]byte(output.Raw), &generated) != nil {
		return
	}
	combined, err := json.Marshal(append(input, generated...))
	if err != nil {
		return
	}
	key.ID = gjson.GetBytes(response, "id").String()
	s.put(key, combined, authID)
}

func replaceRequestBody(c *gin.Context, body []byte) {
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
}
