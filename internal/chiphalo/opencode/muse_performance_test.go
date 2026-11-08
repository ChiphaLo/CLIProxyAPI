package opencode

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func largeNativeHistory(calls int) []byte {
	var input strings.Builder
	input.WriteString(`{"model":"native-fixture","tools":` + museTools + `,"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,`)
	input.WriteString(strings.Repeat("A", 1<<20))
	input.WriteString(`"}]}`)
	for i := 0; i < calls; i++ {
		fmt.Fprintf(&input, `,{"type":"custom_tool_call","id":"ctc_%d","call_id":"call_%d","namespace":"functions","name":"lookup","input":"hello"},{"type":"custom_tool_call_output","call_id":"call_%d","output":"ok"}`, i, i, i)
	}
	input.WriteString(`]}`)
	return []byte(input.String())
}

func TestNativeHistoryCopiesScaleWithPayloadNotCallCount(t *testing.T) {
	allocated := func(calls int) uint64 {
		raw := largeNativeHistory(calls)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		prepared, bridge, err := PrepareNativeResponses(raw, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if err = bridge.BindFinal(prepared); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(prepared)
		return after.TotalAlloc - before.TotalAlloc
	}
	sparse, dense := allocated(8), allocated(96)
	if dense > sparse*2+(4<<20) {
		t.Fatalf("history copies grow with call count: sparse=%d dense=%d", sparse, dense)
	}
}

func TestNativeHistoryPreservesUntouchedItemsVerbatim(t *testing.T) {
	reasoning := `{"type":"reasoning", "id":"rs_owned", "encrypted_content":"opaque", "summary":[]}`
	image := `{"type":"message", "role":"user", "content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}`
	result := `{"type":"custom_tool_call_output","call_id":"call_a","output":[{"type":"input_text","text":"ok"},{"type":"input_image","image_url":"data:image/png;base64,BBBB"}]}`
	raw := []byte(`{"model":"native-fixture","tools":` + museTools + `,"input":[` + reasoning + `,` + image + `,{"type":"custom_tool_call","id":"ctc_a","call_id":"call_a","namespace":"functions","name":"lookup","input":"hello"},` + result + `]}`)
	original := bytes.Clone(raw)
	prepared, bridge, err := PrepareNativeResponses(raw, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, original) {
		t.Fatal("input buffer mutated")
	}
	if err = bridge.BindFinal(prepared); err != nil {
		t.Fatal(err)
	}
	items := ojson.GetBytesNoCopy(prepared, "input").Array()
	if len(items) != 4 || items[0].Raw != reasoning || items[1].Raw != image {
		t.Fatal("opaque reasoning or image bytes changed")
	}
	if items[2].Get("call_id").String() != "call_a" || items[2].Get("name").String() != "functions__lookup" || items[2].Get("arguments").String() != `{"value":"hello"}` || items[2].Get("id").Exists() || items[2].Get("namespace").Exists() {
		t.Fatal("custom history identity changed")
	}
	if items[3].Get("type").String() != "function_call_output" || items[3].Get("output").Raw != ojson.Get(result, "output").Raw || items[3].Get("call_id").String() != "call_a" {
		t.Fatal("image result or pairing changed")
	}
}
