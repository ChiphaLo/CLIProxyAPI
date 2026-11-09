package responses

import (
	"strconv"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
)

// Large media strings stay borrowed from the immutable input until the final
// document. Intermediate message arrays contain small request-private handles.
// All edits and escaping remain owned by ojson.Doc.
type deferredMedia struct {
	prefix string
	values map[string]string
}

func (m *deferredMedia) set(doc *ojson.Doc, data string) {
	if m == nil || len(data) < 4096 {
		_ = doc.Set("source.data", data)
		return
	}
	if m.values == nil {
		m.prefix = "\x00cpa-media:" + uuid.NewString() + ":"
		m.values = map[string]string{}
	}
	handle := m.prefix + strconv.Itoa(len(m.values))
	m.values[handle] = data
	_ = doc.Set("source.data", handle)
}

func (m *deferredMedia) hydrate(doc *ojson.Doc) {
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
