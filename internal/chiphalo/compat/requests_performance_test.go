package compat

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestToolPromotionKeepsHistoryVerbatimWithoutDecodingIt(t *testing.T) {
	image := `{"role":"user", "type":"message", "content":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("A", 8<<20) + `"},{"type":"input_text","text":"<b>&</b>"}]}`
	output := `{"type":"function_call_output", "call_id":"call_1", "output":"1e400 <ok>"}`
	raw := []byte(`{"model":"claude/fixture","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","maximum":1e400}}]},` + image + `,` + output + `],"stream":true}`)
	original := bytes.Clone(raw)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	normalized, err := NormalizeResponsesTools(raw)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, original) {
		t.Fatal("caller request mutated")
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(len(raw))*3 {
		t.Fatalf("history was decoded or copied repeatedly: %d bytes for %d", allocated, len(raw))
	}
	items := ojson.GetBytesNoCopy(normalized, "input").Array()
	if len(items) != 2 || items[0].Raw != image || items[1].Raw != output {
		t.Fatal("untouched history items changed")
	}
	if ojson.GetBytes(normalized, "tools.0.name").String() != "lookup" || ojson.GetBytes(normalized, "tools.0.parameters.maximum").Raw != "1e400" || !ojson.GetBytes(normalized, "stream").Bool() {
		t.Fatalf("promoted tools or request fields changed: %.200s", normalized)
	}
}
