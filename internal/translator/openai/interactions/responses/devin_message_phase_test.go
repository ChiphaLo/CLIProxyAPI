package responses

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func collectDevinStreamEvents(t *testing.T, chunks [][]byte) [][]byte {
	t.Helper()
	var param any
	var out [][]byte
	for _, raw := range chunks {
		out = append(out, ConvertInteractionsResponseToOpenAIResponses(context.Background(), "devin/swe-2", []byte(`{"model":"devin/swe-2"}`), nil, raw, &param)...)
	}
	return out
}

func devinSSE(payload string) []byte {
	return []byte("data: " + payload + "\n\n")
}

func messagePhasesInCompleted(t *testing.T, out [][]byte) map[int]string {
	t.Helper()
	payload := findResponsesEventPayload(out, "response.completed")
	if len(payload) == 0 {
		payload = findResponsesEventPayload(out, "response.incomplete")
	}
	if len(payload) == 0 {
		t.Fatal("missing terminal response event")
	}
	phases := make(map[int]string)
	gjson.GetBytes(payload, "response.output").ForEach(func(key, item gjson.Result) bool {
		if item.Get("type").String() == "message" {
			phases[int(key.Int())] = item.Get("phase").String()
		}
		return true
	})
	return phases
}

// TestDevinStreamMessagePhaseWithToolCall mirrors devin2api semantics: message
// item.done is deferred to the terminal, and a client tool call keeps every
// message at commentary while text deltas still stream eagerly.
func TestDevinStreamMessagePhaseWithToolCall(t *testing.T) {
	out := collectDevinStreamEvents(t, [][]byte{
		devinSSE(`{"event_type":"interaction.created","interaction":{"id":"interaction_1","model":"swe-2"}}`),
		devinSSE(`{"event_type":"step.start","index":0,"step":{"type":"model_output"}}`),
		devinSSE(`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"progress"}}`),
		devinSSE(`{"event_type":"step.stop","index":0}`),
		devinSSE(`{"event_type":"step.start","index":1,"step":{"id":"call_1","type":"function_call","name":"get_weather","arguments":{}}}`),
		devinSSE(`{"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{}"}}`),
		devinSSE(`{"event_type":"step.stop","index":1}`),
		devinSSE(`{"event_type":"interaction.completed","interaction":{"id":"interaction_1","status":"completed","model":"swe-2"}}`),
	})

	var sawDelta, sawItemDoneBeforeTerminal bool
	for _, raw := range out {
		data := ssePayload(raw)
		eventType := gjson.GetBytes(data, "type").String()
		if eventType == "response.output_text.delta" {
			sawDelta = true
		}
		if eventType == "response.output_item.done" && gjson.GetBytes(data, "item.type").String() == "message" {
			sawItemDoneBeforeTerminal = true
			if got := gjson.GetBytes(data, "item.phase").String(); got != "commentary" {
				t.Fatalf("message phase = %q, want commentary", got)
			}
		}
	}
	if !sawDelta {
		t.Fatal("text deltas must stream before the terminal")
	}
	if !sawItemDoneBeforeTerminal {
		t.Fatal("deferred message item.done was never emitted")
	}
	phases := messagePhasesInCompleted(t, out)
	for index, phase := range phases {
		if phase != "commentary" {
			t.Fatalf("response.output[%d].phase = %q, want commentary", index, phase)
		}
	}
	if len(phases) == 0 {
		t.Fatal("completed output has no message item")
	}
}

// TestDevinStreamMessagePhaseFinalAnswer marks the last message of a clean
// tool-less response as final_answer, both in the deferred item.done and in the
// completed response output.
func TestDevinStreamMessagePhaseFinalAnswer(t *testing.T) {
	out := collectDevinStreamEvents(t, [][]byte{
		devinSSE(`{"event_type":"interaction.created","interaction":{"id":"interaction_1","model":"swe-2"}}`),
		devinSSE(`{"event_type":"step.start","index":0,"step":{"type":"model_output"}}`),
		devinSSE(`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"progress"}}`),
		devinSSE(`{"event_type":"step.stop","index":0}`),
		devinSSE(`{"event_type":"step.start","index":1,"step":{"type":"model_output"}}`),
		devinSSE(`{"event_type":"step.delta","index":1,"delta":{"type":"text","text":"done"}}`),
		devinSSE(`{"event_type":"step.stop","index":1}`),
		devinSSE(`{"event_type":"interaction.completed","interaction":{"id":"interaction_1","status":"completed","model":"swe-2"}}`),
	})

	count := 0
	for _, raw := range out {
		data := ssePayload(raw)
		if gjson.GetBytes(data, "type").String() != "response.output_item.done" {
			continue
		}
		if gjson.GetBytes(data, "item.type").String() != "message" {
			continue
		}
		count++
		want := "commentary"
		if gjson.GetBytes(data, "output_index").Int() == 1 {
			want = "final_answer"
		}
		if got := gjson.GetBytes(data, "item.phase").String(); got != want {
			t.Fatalf("message phase = %q, want %s", got, want)
		}
	}
	if count != 2 {
		t.Fatalf("message item.done count = %d, want 2", count)
	}
	phases := messagePhasesInCompleted(t, out)
	if phases[0] != "commentary" || phases[1] != "final_answer" {
		t.Fatalf("completed output phases = %v", phases)
	}
}

// TestDevinStreamMessagePhaseFailed keeps failed responses at commentary.
func TestDevinStreamMessagePhaseFailed(t *testing.T) {
	out := collectDevinStreamEvents(t, [][]byte{
		devinSSE(`{"event_type":"interaction.created","interaction":{"id":"interaction_1","model":"swe-2"}}`),
		devinSSE(`{"event_type":"step.start","index":0,"step":{"type":"model_output"}}`),
		devinSSE(`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"progress"}}`),
		devinSSE(`{"event_type":"step.stop","index":0}`),
		devinSSE(`{"event_type":"interaction.failed","interaction":{"id":"interaction_1","status":"failed","error":{"message":"boom"}}}`),
	})

	found := false
	for _, raw := range out {
		data := ssePayload(raw)
		if gjson.GetBytes(data, "type").String() != "response.output_item.done" {
			continue
		}
		if gjson.GetBytes(data, "item.type").String() == "message" {
			found = true
			if got := gjson.GetBytes(data, "item.phase").String(); got != "commentary" {
				t.Fatalf("failed message phase = %q, want commentary", got)
			}
		}
	}
	if !found {
		t.Fatal("failed stream did not flush pending message items")
	}
}

// TestDevinNonStreamMessagePhases covers the buffered non-stream conversion.
func TestDevinNonStreamMessagePhases(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "final_answer_without_tools",
			raw:  `{"id":"i1","status":"completed","steps":[{"type":"model_output","content":[{"text":"a"}]},{"type":"model_output","content":[{"text":"b"}]}]}`,
			want: []string{"commentary", "final_answer"},
		},
		{
			name: "commentary_with_tool_call",
			raw:  `{"id":"i1","status":"completed","steps":[{"type":"model_output","content":[{"text":"a"}]},{"type":"function_call","id":"call_1","name":"probe","arguments":"{}"}]}`,
			want: []string{"commentary"},
		},
		{
			name: "commentary_when_incomplete",
			raw:  `{"id":"i1","status":"incomplete","steps":[{"type":"model_output","content":[{"text":"a"}]}]}`,
			want: []string{"commentary"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := ConvertInteractionsResponseToOpenAIResponsesNonStream(context.Background(), "devin/swe-2", []byte(`{"model":"devin/swe-2"}`), nil, []byte(test.raw), nil)
			var phases []string
			gjson.GetBytes(out, "output").ForEach(func(_, item gjson.Result) bool {
				if item.Get("type").String() == "message" {
					phases = append(phases, item.Get("phase").String())
				}
				return true
			})
			if len(phases) != len(test.want) {
				t.Fatalf("message phases = %v, want %v. Output: %s", phases, test.want, string(out))
			}
			for i, want := range test.want {
				if phases[i] != want {
					t.Fatalf("message phase[%d] = %q, want %q. Output: %s", i, phases[i], want, string(out))
				}
			}
		})
	}
}

// TestNonDevinStreamUnphased guards the existing behavior for non-Devin models:
// message item.done is still emitted at step.stop without any phase.
func TestNonDevinStreamUnphased(t *testing.T) {
	var param any
	var out [][]byte
	for _, raw := range [][]byte{
		devinSSE(`{"event_type":"interaction.created","interaction":{"id":"i1","model":"gemini-test"}}`),
		devinSSE(`{"event_type":"step.start","index":0,"step":{"type":"model_output"}}`),
		devinSSE(`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"hi"}}`),
		devinSSE(`{"event_type":"step.stop","index":0}`),
		devinSSE(`{"event_type":"interaction.completed","interaction":{"id":"i1","status":"completed","model":"gemini-test"}}`),
	} {
		out = append(out, ConvertInteractionsResponseToOpenAIResponses(context.Background(), "gemini-interactions", []byte(`{"model":"gemini-interactions"}`), nil, raw, &param)...)
	}
	found := false
	for _, raw := range out {
		data := ssePayload(raw)
		if gjson.GetBytes(data, "type").String() != "response.output_item.done" {
			continue
		}
		if gjson.GetBytes(data, "item.type").String() == "message" {
			found = true
			if gjson.GetBytes(data, "item.phase").Exists() {
				t.Fatal("non-Devin message item must not carry a phase")
			}
		}
	}
	if !found {
		t.Fatal("non-Devin message item.done missing")
	}
}
