package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNativeResponsesCodeModeContract(t *testing.T) {
	const description = "Run JavaScript in a fresh V8 isolate. Use tools.exec_command for shell commands and text(value) for output."
	request, _ := json.Marshal(map[string]any{
		"model": "claude-haiku-5-5", "instructions": "Keep original policy.", "input": "Inspect configuration.",
		"tools": []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{
			map[string]any{"type": "custom", "name": "exec", "description": description},
		}}},
	})
	body, bridge, err := PrepareNativeResponses(request, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.BindFinal(body); err != nil {
		t.Fatal(err)
	}
	tool := gjson.GetBytes(body, "tools.0")
	text := tool.Get("description").String()
	if !strings.HasPrefix(text, "CODE-MODE CALL CONTRACT:") || !strings.HasSuffix(text, description) || !strings.Contains(text, "text(r.output)") {
		t.Fatal("code-mode calling and output instructions missing or original documentation lost")
	}
	if tool.Get("name").String() != "functions__exec" || tool.Get("parameters.required.0").String() != "value" || gjson.GetBytes(body, "instructions").String() != nativeCodeModeInstructions+"\n\nKeep original policy." || len(gjson.GetBytes(body, "tools").Array()) != 1 {
		t.Fatal("declarations or caller instructions changed")
	}
	call := []byte(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_bad","call_id":"call_bad","name":"exec_command","arguments":""}}`)
	if _, err := bridge.Event(call); err == nil {
		t.Fatal("nested shell API was authorized as a top-level tool")
	}
	valid := []byte(`{"status":"completed","output":[{"type":"function_call","id":"fc_good","call_id":"call_good","name":"functions__exec","arguments":"{\"value\":\"const r = await tools.exec_command({cmd: 'echo hello'}); text(r.output);\"}"}]}`)
	response, err := bridge.Response(valid)
	if err != nil || gjson.GetBytes(response, "output.0.type").String() != "custom_tool_call" || !strings.Contains(gjson.GetBytes(response, "output.0.input").String(), "text(r.output)") {
		t.Fatalf("declared code-mode call did not round trip: %v", err)
	}
}

func TestNativeResponsesCodeModeLeavesOtherToolsUnchanged(t *testing.T) {
	for _, tool := range []string{
		`{"type":"function","name":"exec","parameters":{"type":"object"},"description":"tools.exec_command text(value)"}`,
		`{"type":"custom","name":"exec","description":"A different custom execution tool."}`,
		`{"type":"custom","name":"other","description":"tools.exec_command text(value)"}`,
	} {
		body, _, err := PrepareNativeResponses([]byte(`{"model":"fixture","tools":[{"type":"namespace","name":"functions","tools":[`+tool+`]}],"input":"hello"}`), nil, "")
		if err != nil || strings.Contains(gjson.GetBytes(body, "tools.0.description").String(), "CODE-MODE CALL CONTRACT:") {
			t.Fatalf("unrelated tool description changed: %v", err)
		}
	}
}
