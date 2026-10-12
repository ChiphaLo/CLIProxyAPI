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
