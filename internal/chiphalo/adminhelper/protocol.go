// Package adminhelper implements the bounded CPA GUI control protocol.
package adminhelper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sync"
	"time"
)

const MaxRequest = 1024 * 1024
const MaxResponse = 65536

var identity = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var requestIdentity = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,64}$`)

var operations = map[string]bool{
	"status": true, "sync_codex": true, "apply_bundle": true, "set_provider_secret": true,
	"delete_provider_secret": true, "probe_provider_models": true, "create_api_key": true,
	"update_api_key": true, "delete_api_key": true, "read_api_keys": true, "gateway_models": true,
	"gui_provider_seed": true, "oauth_reset_credits": true, "consume_oauth_reset_credit": true,
	"workbuddy_credits": true,
}

type Result map[string]any

func failure(message string) Result { return Result{"ok": false, "error": message} }

// object rejects duplicate keys instead of silently using the last value.
func object(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("JSON object required")
	}
	result := map[string]json.RawMessage{}
	for d.More() {
		key, errKey := d.Token()
		name, ok := key.(string)
		if errKey != nil || !ok {
			return nil, errors.New("invalid JSON object")
		}
		if _, exists := result[name]; exists {
			return nil, errors.New("duplicate JSON field")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, errors.New("invalid JSON object")
		}
		result[name] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, errors.New("invalid JSON object")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return result, nil
}

func parse(raw []byte) (string, string, map[string]json.RawMessage, error) {
	fields, err := object(raw)
	if err != nil {
		return "", "", nil, err
	}
	for field := range fields {
		if field != "op" && field != "request_id" && field != "payload" {
			return "", "", nil, errors.New("unknown request fields")
		}
	}
	var op, id string
	if json.Unmarshal(fields["op"], &op) != nil || !operations[op] {
		return "", "", nil, errors.New("invalid operation")
	}
	if json.Unmarshal(fields["request_id"], &id) != nil || !requestIdentity.MatchString(id) {
		return "", "", nil, errors.New("invalid request_id")
	}
	payload := map[string]json.RawMessage{}
	if rawPayload, exists := fields["payload"]; exists {
		payload, err = object(rawPayload)
		if err != nil {
			return "", "", nil, err
		}
	}
	return op, id, payload, nil
}

func encode(result Result) []byte {
	raw, err := json.Marshal(result)
	if err != nil || len(raw)+1 > MaxResponse {
		raw = []byte(`{"ok":false,"error":"response exceeds control protocol limit"}`)
	}
	return append(raw, '\n')
}

// Serve consumes an already secured Unix listener. No TCP fallback is allowed.
func (s *Service) Serve(ctx context.Context, listener *net.UnixListener, allowedUID uint32) error {
	defer s.stopCatalogJobs()
	defer s.stopResetJobs()
	var workers sync.WaitGroup
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-done:
		}
	}()
	defer workers.Wait()
	slots := make(chan struct{}, 16)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("control listener unavailable")
		}
		select {
		case slots <- struct{}{}:
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { <-slots; _ = conn.Close() }()
				s.handle(conn, allowedUID)
			}()
		default:
			_ = conn.Close()
		}
	}
}

func (s *Service) handle(conn *net.UnixConn, allowedUID uint32) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	uid, err := peerUID(conn)
	if err != nil || uid != allowedUID {
		_ = s.audit("", "peer_rejected", "rejected", false, uid)
		_, _ = conn.Write(encode(failure("peer not allowed")))
		return
	}
	raw, err := bufio.NewReader(io.LimitReader(conn, MaxRequest+1)).ReadBytes('\n')
	if len(raw) > MaxRequest || err != nil {
		_ = s.audit("", "protocol_rejected", "rejected", false, uid)
		_, _ = conn.Write(encode(failure("bounded newline-delimited JSON required")))
		return
	}
	op, id, payload, err := parse(raw[:len(raw)-1])
	if err != nil {
		_ = s.audit("", "protocol_rejected", "rejected", false, uid)
		_, _ = conn.Write(encode(failure(err.Error())))
		return
	}
	if op == "apply_bundle" {
		// Only this bounded local management operation needs a longer reply
		// window; slow/unframed reads still retain the ten-second limit.
		_ = conn.SetWriteDeadline(time.Now().Add(85 * time.Second))
	}
	budget := 10 * time.Second
	if op == "apply_bundle" {
		budget = 70 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if s.audit(id, op, "started", false, uid) != nil {
		_, _ = conn.Write(encode(failure("audit unavailable; operation not started")))
		return
	}
	result := s.dispatchContext(ctx, op, payload)
	// Audit metadata only: payloads, keys and provider secrets never enter logs.
	if s.audit(id, op, "completed", result["ok"] == true, uid) != nil {
		result = failure("audit unavailable; operation outcome must be checked")
	}
	_, _ = conn.Write(encode(result))
}
