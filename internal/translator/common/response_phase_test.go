package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestJSONResponseMessagePhases(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"status":"completed","output":[{"type":"message","role":"assistant","content":[]}]}`, "final_answer"},
		{`{"status":"incomplete","output":[{"type":"message","role":"assistant","content":[]}]}`, "commentary"},
		{`{"status":"failed","output":[{"type":"message","role":"assistant","content":[]}]}`, "commentary"},
		{`{"status":"completed","output":[{"type":"message","role":"assistant","content":[]},{"type":"function_call","name":"probe","arguments":"{}"}]}`, "commentary"},
		{`{"status":"completed","output":[{"type":"message","role":"assistant","content":[]},{"type":"custom_tool_call","name":"exec","input":"code"}]}`, "commentary"},
	} {
		out := SetResponseMessagePhases([]byte(tc.body))
		if gjson.GetBytes(out, "output.0.phase").String() != tc.want || gjson.GetBytes(out, "output.1.phase").Exists() {
			t.Fatalf("unexpected phase assignment: %s", out)
		}
	}
	body := []byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[]},{"type":"reasoning","summary":[]},{"type":"message","role":"assistant","content":[]}]}`)
	out := SetResponseMessagePhases(body)
	if gjson.GetBytes(out, "output.0.phase").String() != "commentary" || gjson.GetBytes(out, "output.2.phase").String() != "final_answer" || gjson.GetBytes(out, "output.1.phase").Exists() {
		t.Fatal("only the final text message should close the turn")
	}
}
