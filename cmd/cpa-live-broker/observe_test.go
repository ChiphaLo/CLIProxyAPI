package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestResponseObserverPreservesWireAndRedactsValues(t *testing.T) {
	wire := "data: {\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"delta\":{\"reasoning_content\":\"PRIVATE-THOUGHT\",\"tool_calls\":[{\"id\":\"call_test\",\"function\":{\"name\":\"exec\",\"arguments\":\"PRIVATE-CODE\"}}]}}]}\r\n\r\n" +
		"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5,\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n" +
		"data: [DONE]\n\n"
	logs := []map[string]any{}
	o := newResponseObserver(io.NopCloser(strings.NewReader(wire)), 200, true, func(m map[string]any) { logs = append(logs, m) })
	var got bytes.Buffer
	p := make([]byte, 1)
	for {
		n, err := o.Read(p)
		got.Write(p[:n])
		if err != nil {
			if err != io.EOF {
				t.Fatal(err)
			}
			break
		}
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if got.String() != wire || len(logs) != 1 {
		t.Fatal("observer changed wire bytes or emitted duplicate completion")
	}
	m := logs[0]
	if m["tool_call_count"] != 1 || m["done_marker"] != true || m["json_frames"] != 2 {
		t.Fatalf("unexpected metadata: %v", m)
	}
	if m["usage"].(map[string]int64)["completion_tokens_details.reasoning_tokens"] != 3 {
		t.Fatal("raw usage metadata missing")
	}
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), "PRIVATE-") || strings.Contains(string(raw), "call_test") {
		t.Fatal("observer leaked private values")
	}
}

func TestResponseObserverOversizeIsTransparentAndBounded(t *testing.T) {
	wire := "data: " + strings.Repeat("x", 80*1024) + "\n\ndata: [DONE]\n\n"
	var meta map[string]any
	o := newResponseObserver(io.NopCloser(strings.NewReader(wire)), 200, true, func(m map[string]any) { meta = m })
	raw, err := io.ReadAll(o)
	if err != nil || string(raw) != wire || meta["oversize_observer_frames"] != 1 || meta["done_marker"] != true || cap(o.pending) > 128*1024 {
		t.Fatalf("oversize observation is not bounded/transparent: %v %v", err, meta)
	}
}

func TestRequestHistoryShape(t *testing.T) {
	meta := requestMetadata([]byte(`{"messages":[{"role":"assistant","reasoning_content":"secret","tool_calls":[{"id":"a"},{"id":"b"}]},{"role":"tool","tool_call_id":"a"},{"role":"tool","tool_call_id":"b"}]}`))
	if meta["history_tool_calls"] != 2 || meta["history_tool_results"] != 2 || meta["unanswered_history_calls"] != 0 || meta["orphan_history_results"] != 0 || meta["reasoning_messages"] != 1 {
		t.Fatalf("unexpected history shape: %v", meta)
	}
	bad := requestMetadata([]byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"a"}]},{"role":"tool","tool_call_id":"other"}]}`))
	if bad["unanswered_history_calls"] != 1 || bad["orphan_history_results"] != 1 {
		t.Fatal("unpaired history was not visible")
	}
}

func TestResponsesObserverKeepsNamespaceAndCustomEvents(t *testing.T) {
	var meta map[string]any
	wire := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"custom_tool_call\",\"namespace\":\"functions\",\"name\":\"apply_patch\",\"call_id\":\"c1\",\"input\":\"PRIVATE-PATCH\"}}\n\n" +
		"data: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"PRIVATE-DELTA\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"test-model\",\"usage\":{\"input_tokens\":3,\"output_tokens\":7,\"output_tokens_details\":{\"reasoning_tokens\":4}}}}\n\n"
	o := newResponseObserver(io.NopCloser(strings.NewReader(wire)), 200, true, func(m map[string]any) { meta = m })
	got, err := io.ReadAll(o)
	if err != nil || string(got) != wire || meta["event"] != "live_codex_response" || meta["tool_call_count"] != 1 || meta["tool_names"].([]string)[0] != "functions.apply_patch" {
		t.Fatalf("unexpected Responses shape: %v %v", err, meta)
	}
	encoded, _ := json.Marshal(meta)
	if strings.Contains(string(encoded), "PRIVATE-") || !strings.Contains(string(encoded), "response.completed") {
		t.Fatal("response event metadata missing or private values leaked")
	}
}

func TestResponsesRequestMetadataIsReadOnly(t *testing.T) {
	raw := []byte(`{"tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"},{"type":"custom","name":"apply_patch"}]}],"input":[{"type":"custom_tool_call_output","call_id":"c1","output":"PRIVATE-OUTPUT"}]}`)
	original := append([]byte(nil), raw...)
	meta := responsesRequestMetadata(raw)
	if !bytes.Equal(raw, original) || len(meta["tools"].([]map[string]string)) != 3 {
		t.Fatal("request metadata collection changed the payload or lost a tool")
	}
	encoded, _ := json.Marshal(meta)
	if strings.Contains(string(encoded), "PRIVATE-") || !strings.Contains(string(encoded), "custom_tool_call_output") {
		t.Fatal("request metadata missing or private values leaked")
	}
}
