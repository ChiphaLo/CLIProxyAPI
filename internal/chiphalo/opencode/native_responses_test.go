package opencode

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNativeResponsesRetiredToolHistory(t *testing.T) {
	for _, kind := range []string{"function_call", "custom_tool_call"} {
		t.Run(kind, func(t *testing.T) {
			for _, tools := range []string{`[]`, `[{"type":"function","name":"ping","parameters":{"type":"object"}}]`} {
				raw := []byte(fmt.Sprintf(`{"model":"claude-sonnet-5-5","tools":%s,"input":[{"type":%q,"namespace":"retired","name":"exec","call_id":"call_old","arguments":"{\"cmd\":\"true\"}","input":"old result"},{"type":"custom_tool_call_output","call_id":"call_old","output":"done"}]}`, tools, kind))
				body, bridge, err := PrepareNativeResponses(raw, nil, "")
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(body, "tools").Raw != tools || gjson.GetBytes(body, "input.0.name").String() != "retired__exec" || gjson.GetBytes(body, "input.0.call_id").String() != "call_old" || gjson.GetBytes(body, "input.1.call_id").String() != "call_old" || gjson.GetBytes(body, "input.1.output").String() != "done" || gjson.GetBytes(body, "input.1.type").String() != "function_call_output" {
					t.Fatalf("history or declarations changed: %s", body)
				}
				if kind == "custom_tool_call" && gjson.Get(gjson.GetBytes(body, "input.0.arguments").String(), "value").String() != "old result" {
					t.Fatalf("custom history envelope lost: %s", body)
				}
				if err := bridge.BindFinal(body); err != nil {
					t.Fatal(err)
				}
				response := []byte(`{"id":"resp_fixture","output":[{"type":"function_call","id":"fc_old","name":"retired__exec","call_id":"call_new","arguments":"{}"}]}`)
				if _, err := bridge.Response(response); err == nil {
					t.Fatal("retired history tool became callable")
				}
				if _, _, err := PrepareMuse(raw, nil, "fixture"); err == nil {
					t.Fatal("Muse history restrictions changed")
				}
			}
		})
	}
}

func TestNativeResponsesNamespaceCustomReplay(t *testing.T) {
	raw := []byte(`{"model":"fixture","tool_choice":"required","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"ping","description":"fixture"}]}],"input":[{"type":"reasoning","id":"rs_fixture","encrypted_content":"opaque-provider-signed-data"},{"type":"custom_tool_call","namespace":"functions","name":"ping","call_id":"call_prior","input":"hello"},{"type":"custom_tool_call_output","call_id":"call_prior","output":"hello"}]}`)
	body, b, err := PrepareNativeResponses(raw, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "tools.0.name").String() != "functions__ping" || gjson.GetBytes(body, "tools.0.type").String() != "function" || gjson.GetBytes(body, "input.0.encrypted_content").String() != "opaque-provider-signed-data" || gjson.GetBytes(body, "input.2.type").String() != "function_call_output" {
		t.Fatalf("invalid reversible envelope: %s", body)
	}
	if err := b.BindFinal(body); err != nil {
		t.Fatal(err)
	}
	out, err := b.Response([]byte(`{"id":"resp_fixture","object":"response","status":"completed","output":[{"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"functions__ping","arguments":"{\"value\":\"hello\"}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "output.0.type").String() != "custom_tool_call" || gjson.GetBytes(out, "output.0.namespace").String() != "functions" || gjson.GetBytes(out, "output.0.name").String() != "ping" || gjson.GetBytes(out, "output.0.input").String() != "hello" {
		t.Fatalf("restoration failed: %s", out)
	}
}
