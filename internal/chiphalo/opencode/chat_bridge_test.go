package opencode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func newBridge(t *testing.T, choice any) *ChatBridge {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "exec", "parameters": json.RawMessage(execSchema)}}}, "tool_choice": choice})
	bridge, err := NewChatBridge(WithRecovery(context.Background()), raw)
	if err != nil {
		t.Fatal(err)
	}
	return bridge
}

func chatEvent(text any, finish any, calls any) []byte {
	delta := map[string]any{"content": text}
	if calls != nil {
		delta["tool_calls"] = calls
	}
	raw, _ := json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "model": "reported-model", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	return raw
}

func TestChatBridgeOptInAndFinalPayloadPolicy(t *testing.T) {
	raw := []byte(`{"tools":[{"function":{"name":"bad"}}]}`)
	bridge, err := NewChatBridge(context.Background(), raw)
	if err != nil || bridge != nil {
		t.Fatal("default upstream behavior was changed")
	}
	if result, err := bridge.Response(raw); err != nil || string(result) != string(raw) {
		t.Fatal("nil bridge rewrote bytes")
	}
	text := dsmlRoot(dsmlInvoke("exec", "input", "text(1);", "true"))
	for _, choice := range []any{"none", map[string]any{"type": "function", "function": map[string]any{"name": "not_declared"}}} {
		bridge := newBridge(t, choice)
		event, err := bridge.Event(chatEvent(text, "stop", nil))
		if err != nil || gjson.GetBytes(event, "choices.0.delta.content").String() != FormatError || gjson.GetBytes(event, "choices.0.delta.tool_calls").Exists() {
			t.Fatal("tool_choice barrier bypassed")
		}
	}
	bridge, err = NewChatBridge(WithRecovery(context.Background()), []byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"exec"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	event, _ := bridge.Event(chatEvent(text, "stop", nil))
	if gjson.GetBytes(event, "choices.0.delta.tool_calls").Exists() {
		t.Fatal("history authorized a tool")
	}
}

func TestChatBridgeEveryUnicodeSplitWaitsForWholeBatch(t *testing.T) {
	code := "// orchestration remains JavaScript\nconst x = `λ\\n`; text(x);"
	text := dsmlRoot(dsmlInvoke("exec", "input", code, "true"))
	runes := []rune(text)
	for split := 1; split < len(runes); split++ {
		bridge := newBridge(t, "auto")
		first, err := bridge.Event(chatEvent(string(runes[:split]), nil, nil))
		if err != nil || gjson.GetBytes(first, "choices.0.delta.tool_calls").Exists() || gjson.GetBytes(first, "choices.0.delta.content").Exists() {
			t.Fatalf("DSML leaked before whole-batch validation at %d: %s %v", split, first, err)
		}
		last, err := bridge.Event(chatEvent(string(runes[split:]), "stop", nil))
		if err != nil || gjson.GetBytes(last, "choices.0.finish_reason").String() != "tool_calls" || gjson.GetBytes(last, "choices.0.delta.tool_calls.0.function.name").String() != "exec" {
			t.Fatalf("split %d recovery failed: %s %v", split, last, err)
		}
		arguments := gjson.GetBytes(last, "choices.0.delta.tool_calls.0.function.arguments").String()
		if gjson.Get(arguments, "input").String() != code {
			t.Fatal("JavaScript arguments changed")
		}
		final, err := bridge.Finish()
		if err != nil || len(final) != 0 {
			t.Fatal("duplicate terminal event")
		}
	}
}

func TestChatBridgeRejectsWholeInvalidBatchAndIncompleteTermination(t *testing.T) {
	valid := dsmlInvoke("exec", "input", "text(1);", "true")
	for _, scenario := range []string{"undeclared-second-call", "bad-schema", "missing-terminal", "length", "overflow"} {
		t.Run(scenario, func(t *testing.T) {
			bridge := newBridge(t, "auto")
			text, finish := dsmlRoot(valid), "stop"
			switch scenario {
			case "undeclared-second-call":
				text = dsmlRoot(valid + dsmlInvoke("unknown", "input", "bad", "true"))
			case "bad-schema":
				text = dsmlRoot(dsmlInvoke("exec", "command", "bash", "true"))
			case "length":
				finish = "length"
			case "overflow":
				text = dsmlRoot(dsmlInvoke("exec", "input", strings.Repeat("x", MaxChars), "true"))
			}
			var event []byte
			var err error
			if scenario == "missing-terminal" {
				first, errFirst := bridge.Event(chatEvent(text, nil, nil))
				if errFirst != nil || gjson.GetBytes(first, "choices.0.delta.tool_calls").Exists() {
					t.Fatal("unterminated batch escaped")
				}
				event, err = bridge.Finish()
			} else {
				event, err = bridge.Event(chatEvent(text, finish, nil))
			}
			if err != nil || gjson.GetBytes(event, "choices.0.delta.content").String() != FormatError || gjson.GetBytes(event, "choices.0.delta.tool_calls").Exists() {
				t.Fatalf("unsafe partial recovery: %s %v", event, err)
			}
		})
	}
}

func TestChatBridgeNormalTextAndNativeCallsUnchanged(t *testing.T) {
	bridge := newBridge(t, "auto")
	first, err := bridge.Event(chatEvent(" \n", nil, nil))
	if err != nil || gjson.GetBytes(first, "choices.0.delta.content").Exists() {
		t.Fatal("leading whitespace should be held only until type is known")
	}
	text := "Example:\n```\n" + dsmlRoot(dsmlInvoke("exec", "input", "text(1);", "true")) + "\n```"
	last, err := bridge.Event(chatEvent(text, "stop", nil))
	if err != nil || gjson.GetBytes(last, "choices.0.delta.content").String() != " \n"+text || gjson.GetBytes(last, "choices.0.delta.tool_calls").Exists() {
		t.Fatal("quoted example became a tool")
	}
	bridge = newBridge(t, "auto")
	native := []any{map[string]any{"index": 0, "id": "native", "type": "function", "function": map[string]any{"name": "exec", "arguments": `{"input":"text(1);"}`}}}
	event, err := bridge.Event(chatEvent(dsmlRoot(dsmlInvoke("unknown", "input", "bad", "true")), "tool_calls", native))
	if err != nil || gjson.GetBytes(event, "choices.0.delta.tool_calls.#").Int() != 1 || gjson.GetBytes(event, "choices.0.delta.tool_calls.0.id").String() != "native" {
		t.Fatal("text overrode native tool_calls")
	}
}

func TestChatBridgeNonStreamAndUsageArePreserved(t *testing.T) {
	bridge := newBridge(t, "auto")
	text := dsmlRoot(dsmlInvoke("exec", "input", "text(1);", "true"))
	raw, _ := json.Marshal(map[string]any{"model": "reported-model", "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20}, "choices": []any{map[string]any{"message": map[string]any{"content": text}, "finish_reason": "stop"}}})
	out, err := bridge.Response(raw)
	if err != nil || gjson.GetBytes(out, "usage").Raw != gjson.GetBytes(raw, "usage").Raw || gjson.GetBytes(out, "model").String() != "reported-model" || gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.name").String() != "exec" {
		t.Fatalf("response/usage damaged: %s %v", out, err)
	}
	usage := []byte(`{"model":"reported-model","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
	if out, err := bridge.Event(usage); err != nil || string(out) != string(usage) {
		t.Fatal("standalone usage event changed")
	}
}

func TestChatBridgeStepRepeatedTerminalUsage(t *testing.T) {
	usage := `{"prompt_tokens":261,"completion_tokens":24,"total_tokens":285,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"completion_tokens_details":{"reasoning_tokens":1}}`
	for _, mode := range []string{"native", "dsml", "text"} {
		t.Run(mode, func(t *testing.T) {
			bridge := newBridge(t, "auto")
			var native any
			text, finish := "", "tool_calls"
			switch mode {
			case "native":
				native = []any{map[string]any{"index": 0, "id": "step-native", "type": "function", "function": map[string]any{"name": "exec", "arguments": `{"input":"text(1);"}`}}}
			case "dsml":
				text, finish = dsmlRoot(dsmlInvoke("exec", "input", "text(1);", "true")), "stop"
			case "text":
				text, finish = "OK", "stop"
			}
			if _, err := bridge.Event(chatEvent(text, finish, native)); err != nil {
				t.Fatal(err)
			}
			late := []byte(`{"id":"fixture","object":"chat.completion.chunk","model":"reported-model","choices":[{"index":0,"delta":{"role":"assistant","content":null},"finish_reason":"` + finish + `"}],"usage":` + usage + `}`)
			out, err := bridge.Event(late)
			if err != nil || gjson.GetBytes(out, "choices").Raw != "[]" || gjson.GetBytes(out, "usage").Raw != usage {
				t.Fatalf("late usage rejected or changed: %s %v", out, err)
			}
			if last, err := bridge.Finish(); err != nil || len(last) != 0 {
				t.Fatalf("terminal duplicated: %s %v", last, err)
			}
		})
	}
}

func TestChatBridgeRepeatedTerminalUsageStillRejectsNewOutput(t *testing.T) {
	for _, delta := range []string{
		`{"content":"late text"}`, `{"reasoning_content":"late reasoning"}`,
		`{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}`,
		`{"function_call":{"name":"exec"}}`, `{"role":"user"}`, `{"unknown":null}`, `[]`,
	} {
		t.Run(delta, func(t *testing.T) {
			bridge := newBridge(t, "auto")
			_, _ = bridge.Event(chatEvent("OK", "stop", nil))
			late := []byte(`{"choices":[{"index":0,"delta":` + delta + `,"finish_reason":"stop"}],"usage":{"prompt_tokens":1}}`)
			if _, err := bridge.Event(late); err == nil {
				t.Fatal("post-terminal output was accepted")
			}
		})
	}
	for _, late := range []string{
		`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":1}}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop","message":{"content":"late"}}],"usage":{"prompt_tokens":1}}`,
	} {
		bridge := newBridge(t, "auto")
		_, _ = bridge.Event(chatEvent("OK", "stop", nil))
		if _, err := bridge.Event([]byte(late)); err == nil {
			t.Fatalf("invalid late terminal accepted: %s", late)
		}
	}
}

func TestChatBridgeRepeatedTerminalDoesNotDropActiveChoices(t *testing.T) {
	bridge := newBridge(t, "auto")
	_, _ = bridge.Event(chatEvent("OK", "stop", nil))
	raw := []byte(`{"choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"},{"index":1,"delta":{"content":"active"},"finish_reason":null}],"usage":{"prompt_tokens":1}}`)
	out, err := bridge.Event(raw)
	if err != nil || gjson.GetBytes(out, "choices.#").Int() != 1 || gjson.GetBytes(out, "choices.0.index").Int() != 1 || gjson.GetBytes(out, "choices.0.delta.content").String() != "active" {
		t.Fatalf("active choice lost: %s %v", out, err)
	}
}
