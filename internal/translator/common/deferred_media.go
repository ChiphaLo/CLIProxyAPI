package common

import (
	"bytes"
	"context"
	"strconv"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
)

// deferredMediaMinBytes is the smallest media string worth replacing.
const deferredMediaMinBytes = 4096

// DeferredMedia keeps large media strings, borrowed from the immutable request
// input, out of intermediate documents. Those documents carry short
// request-private handles until the final document is built.
type DeferredMedia struct {
	prefix string
	values map[string]string
}

type deferredMediaContextKey struct{}

// WithDeferredMedia returns ctx carrying media. A translator that finds it
// leaves its handles in the translated body; the caller then owns hydration.
func WithDeferredMedia(ctx context.Context, media *DeferredMedia) context.Context {
	return context.WithValue(ctx, deferredMediaContextKey{}, media)
}

// DeferredMediaFromContext returns the media store carried by ctx, if any.
func DeferredMediaFromContext(ctx context.Context) *DeferredMedia {
	if ctx == nil {
		return nil
	}
	media, _ := ctx.Value(deferredMediaContextKey{}).(*DeferredMedia)
	return media
}

// Handle returns the handle standing in for data, or data itself when it is
// too small to defer.
func (m *DeferredMedia) Handle(data string) string {
	if m == nil || len(data) < deferredMediaMinBytes {
		return data
	}
	if m.values == nil {
		m.prefix = "\x00cpa-media:" + uuid.NewString() + ":"
		m.values = map[string]string{}
	}
	handle := m.prefix + strconv.Itoa(len(m.values))
	m.values[handle] = data
	return handle
}

// Resolve returns the media a handle stands for, or value when it is not a
// handle.
func (m *DeferredMedia) Resolve(value string) string {
	if m != nil {
		if data, ok := m.values[value]; ok {
			return data
		}
	}
	return value
}

// Hydrate writes the deferred values back into the image and document blocks
// of a Claude Messages document.
func (m *DeferredMedia) Hydrate(doc *ojson.Doc) {
	if m == nil || len(m.values) == 0 {
		return
	}
	type edit struct{ path, value string }
	var edits []edit
	var blocks func(ojson.Result, string)
	blocks = func(parts ojson.Result, path string) {
		parts.ForEach(func(index, part ojson.Result) bool {
			p := path + "." + strconv.Itoa(int(index.Int()))
			switch part.Get("type").String() {
			case "image", "document":
				if value, ok := m.values[part.Get("source.data").String()]; ok {
					edits = append(edits, edit{p + ".source.data", value})
				}
			case "tool_result":
				if content := part.Get("content"); content.IsArray() {
					blocks(content, p+".content")
				}
			}
			return true
		})
	}
	doc.Get("messages").ForEach(func(index, message ojson.Result) bool {
		if content := message.Get("content"); content.IsArray() {
			blocks(content, "messages."+strconv.Itoa(int(index.Int()))+".content")
		}
		return true
	})
	if system := doc.Get("system"); system.IsArray() {
		blocks(system, "system")
	}
	for _, edit := range edits {
		_ = doc.Set(edit.path, edit.value)
	}
}

// HydrateJSON returns body with every handle string replaced by the JSON
// string its value encodes to, wherever the handle ended up. A body without
// handles is returned unchanged.
func (m *DeferredMedia) HydrateJSON(body []byte) []byte {
	if m == nil || len(m.values) == 0 {
		return body
	}
	marker := []byte(`"\u0000` + m.prefix[1:])
	if !bytes.Contains(body, marker) {
		return body
	}
	size := len(body)
	for _, value := range m.values {
		size += len(value)
	}
	out := make([]byte, 0, size)
	rest := body
	for {
		start := bytes.Index(rest, marker)
		if start < 0 {
			break
		}
		digits := start + len(marker)
		end := digits
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		value, ok := "", false
		if end > digits && end < len(rest) && rest[end] == '"' {
			value, ok = m.values[m.prefix+string(rest[digits:end])]
		}
		if !ok {
			out = append(out, rest[:digits]...)
			rest = rest[digits:]
			continue
		}
		out = append(out, rest[:start]...)
		out = appendDocJSONString(out, value)
		rest = rest[end+1:]
	}
	return append(out, rest...)
}

// appendDocJSONString appends value encoded exactly as ojson.Doc.Set encodes
// a string.
func appendDocJSONString(dst []byte, value string) []byte {
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < 0x20 || c >= 0x7f || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			encoded, err := ojson.SetBytes([]byte(`[0]`), "0", value)
			if err != nil {
				break
			}
			return append(dst, encoded[1:len(encoded)-1]...)
		}
	}
	dst = append(dst, '"')
	dst = append(dst, value...)
	return append(dst, '"')
}
