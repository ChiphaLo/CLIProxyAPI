package opencode

import (
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

const museTools = `[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"lookup","format":{"type":"text"}}]}]`
const museRequest = `{"model":"muse-spark-1.3-contributor","input":[{"type":"additional_tools","role":"developer","tools":` + museTools + `},{"type":"message","role":"user","content":[{"type":"input_text","text":"fixture"}]}],"tool_choice":"auto"}`
const museCallItem = `{"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"functions__lookup","arguments":"{\"input\":\"7\"}"}`

func TestMuseNativeToolsRoundTrip(t *testing.T) {
	raw, b, err := PrepareMuse([]byte(museRequest), nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(raw, "input.#").Int() != 1 || gjson.GetBytes(raw, "input.0.role").String() != "user" || gjson.GetBytes(raw, "tools.0.name").String() != "functions__lookup" {
		t.Fatalf("incorrect request: %s", raw)
	}
	if err := b.BindFinal(raw); err != nil {
		t.Fatal(err)
	}
	result, err := b.Response([]byte(`{"object":"response","status":"completed","output":[` + museCallItem + `],"usage":{"input_tokens":1,"output_tokens":9,"output_tokens_details":{"reasoning_tokens":8}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(result, "output.0.type").String() != "custom_tool_call" || gjson.GetBytes(result, "output.0.namespace").String() != "functions" || gjson.GetBytes(result, "output.0.name").String() != "lookup" || gjson.GetBytes(result, "output.0.input").String() != "7" || gjson.GetBytes(result, "output.0.call_id").String() != "call_fixture" {
		t.Fatalf("incorrect response: %s", result)
	}
	if gjson.GetBytes(result, "usage.output_tokens").Int() != 9 {
		t.Fatal("usage modified")
	}
	history := `{"model":"muse-spark-1.3-contributor","tools":` + museTools + `,"input":[` + gjson.GetBytes(result, "output.0").Raw + `,{"type":"custom_tool_call_output","call_id":"call_fixture","output":"42"}]}`
	prepared, _, err := PrepareMuse([]byte(history), nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(prepared, "input.0.arguments").String() != `{"input":"7"}` || gjson.GetBytes(prepared, "input.1.type").String() != "function_call_output" || gjson.GetBytes(prepared, "input.1.call_id").String() != "call_fixture" {
		t.Fatalf("history corrupt: %s", prepared)
	}
}

func TestMuseToolChoiceAndFinalBarrier(t *testing.T) {
	for _, choice := range []string{`"required"`, `{"type":"function","name":"lookup"}`} {
		if _, _, err := PrepareMuse([]byte(strings.Replace(museRequest, `"auto"`, choice, 1)), nil, "fixture"); err == nil {
			t.Fatal("unsupported choice accepted")
		}
	}
	raw, b, err := PrepareMuse([]byte(strings.Replace(museRequest, `"auto"`, `"none"`, 1)), nil, "fixture")
	if err != nil || gjson.GetBytes(raw, "tools").Exists() {
		t.Fatalf("none did not remove tools: %s %v", raw, err)
	}
	if err := b.BindFinal(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Response([]byte(`{"object":"response","status":"completed","output":[` + museCallItem + `]}`)); err == nil {
		t.Fatal("prohibited call restored")
	}
	_, b, err = PrepareMuse([]byte(museRequest), nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if b.BindFinal([]byte(`{"tool_choice":"required"}`)) == nil {
		t.Fatal("final rule overwritten")
	}
}

func TestMuseReasoningReferencesAreScopedAndPortable(t *testing.T) {
	s := NewMuseReferences()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	raw := []byte(`{"model":"muse-spark-1.3-contributor","input":[{"type":"reasoning","id":"rs_foreign","summary":[{"type":"summary_text","text":"portable history"}]}]}`)
	got, b, err := PrepareMuse(raw, s, "caller-A/account-A/session-A")
	if err != nil || gjson.GetBytes(got, "input.0.id").Exists() || gjson.GetBytes(got, "input.0.summary.0.text").String() != "portable history" {
		t.Fatal("foreign history was not safely replayed")
	}
	_, err = b.Response([]byte(`{"object":"response","status":"completed","output":[{"type":"reasoning","id":"rs_foreign","summary":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _, err = PrepareMuse(raw, s, "caller-A/account-A/session-A")
	if err != nil || gjson.GetBytes(got, "input.0.id").String() != "rs_foreign" {
		t.Fatal("owned Muse identity lost")
	}
	for _, scope := range []string{"caller-B/account-A/session-A", "caller-A/account-B/session-A", "caller-A/account-A/session-B"} {
		got, _, err = PrepareMuse(raw, s, scope)
		if err != nil || gjson.GetBytes(got, "input.0.id").Exists() {
			t.Fatal("foreign identity reused")
		}
	}
	now = now.Add(16 * time.Minute)
	got, _, err = PrepareMuse(raw, s, "caller-A/account-A/session-A")
	if err != nil || gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatal("expired identity reused")
	}
	encrypted := strings.Replace(string(raw), `"summary":`, `"encrypted_content":"opaque-private-state","summary":`, 1)
	if _, _, err = PrepareMuse([]byte(encrypted), s, "other"); err == nil {
		t.Fatal("foreign opaque reasoning silently discarded")
	}
}

func TestMuseSSECustomArgumentsBufferedAndValidated(t *testing.T) {
	raw, b, err := PrepareMuse([]byte(museRequest), nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.BindFinal(raw); err != nil {
		t.Fatal(err)
	}
	added := `{"type":"response.output_item.added","sequence_number":12,"output_index":0,"item":{"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"functions__lookup","arguments":""}}`
	events, err := b.Event([]byte(added))
	if err != nil || gjson.GetBytes(events[0], "item.type").String() != "custom_tool_call" {
		t.Fatal("custom added event missing")
	}
	events, err = b.Event([]byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_fixture","delta":"{\"input\":\"7\"}"}`))
	if err != nil || len(events) != 0 {
		t.Fatal("unvalidated custom input emitted")
	}
	events, err = b.Event([]byte(`{"type":"response.function_call_arguments.done","sequence_number":14,"item_id":"fc_fixture","arguments":"{\"input\":\"7\"}"}`))
	if err != nil || len(events) != 2 || gjson.GetBytes(events[0], "delta").String() != "7" || gjson.GetBytes(events[1], "type").String() != "response.custom_tool_call_input.done" || gjson.GetBytes(events[1], "sequence_number").Int() != 2 {
		t.Fatalf("bad custom input events: %s %v", events, err)
	}
}

func TestMuseRejectsUserDeclarationsAndInvalidOutput(t *testing.T) {
	raw, b, err := PrepareMuse([]byte(strings.Replace(museRequest, `"role":"developer"`, `"role":"user"`, 1)), nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.BindFinal(raw); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Response([]byte(`{"output":[` + museCallItem + `]}`)); err == nil {
		t.Fatal("user declaration authorized output")
	}
	for _, args := range []string{`{"input":7}`, `{"input":"x","input":"y"}`, `{"input":"7","extra":1}`} {
		raw, b, err = PrepareMuse([]byte(museRequest), nil, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if err = b.BindFinal(raw); err != nil {
			t.Fatal(err)
		}
		item := strings.Replace(museCallItem, `{\"input\":\"7\"}`, strings.ReplaceAll(args, `"`, `\"`), 1)
		if _, err = b.Response([]byte(`{"output":[` + item + `]}`)); err == nil {
			t.Fatalf("invalid arguments accepted: %s", args)
		}
	}
}
