package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	responsesclaude "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeResizeNoticeFullStreamingExecutorWire(t *testing.T) {
	const notice = "<image_resize_notice>\nImage 1 of 1 in the preceding tool output was resized from 4282x2000 to 2048x957 pixels.\n</image_resize_notice>"
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"} {
		t.Run(model, func(t *testing.T) {
			var seen []byte
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				seen, _ = io.ReadAll(r.Body)
				events := `data: {"type":"message_start","message":{"id":"msg_fixture","model":"` + model + `","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n" +
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}` + "\n\n" +
					`data: {"type":"content_block_stop","index":0}` + "\n\n" +
					`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
					`data: {"type":"message_stop"}` + "\n\n"
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events)), Request: r}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
			auth := &cliproxyauth.Auth{ID: "resize-wire-fixture", Attributes: map[string]string{"api_key": "sk-ant-oat-resize-wire-fixture", "cloak_mode": "always"}, Metadata: claudeOAuthTestMetadata()}
			items := []any{
				map[string]any{"type": "message", "role": "developer", "content": "Stable developer policy."},
				map[string]any{"type": "message", "role": "user", "content": "Initial request."},
				map[string]any{"type": "function_call", "call_id": "call_resize", "name": "fixture", "arguments": "{}"},
				map[string]any{"type": "function_call_output", "call_id": "call_resize", "output": "tool result"},
				map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": notice}}},
			}
			raw, _ := json.Marshal(map[string]any{"model": model, "input": items, "tools": []any{map[string]any{"type": "function", "name": "fixture", "parameters": map[string]any{"type": "object"}}}})
			result, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: raw}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
			if err != nil {
				t.Fatal(err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
			}
			messages := gjson.GetBytes(seen, "messages").Array()
			index := -1
			for i, m := range messages {
				if m.Get("content.0.text").String() == notice {
					index = i
				}
			}
			if index < 1 || messages[index].Get("role").String() != "system" || messages[index-1].Get("content.0.type").String() != "tool_result" {
				t.Fatal("final upstream request moved or downgraded the notice")
			}
		})
	}
}

func TestClaudeResizeNoticePreservesNativeCachePrefix(t *testing.T) {
	const notice = "<image_resize_notice>\nImage 1 of 1 in the preceding tool output was resized from 4282x2000 to 2048x957 pixels.\n</image_resize_notice>"
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"} {
		t.Run(model, func(t *testing.T) {
			items := []any{
				map[string]any{"type": "message", "role": "developer", "content": "Stable developer policy."},
				map[string]any{"type": "message", "role": "user", "content": "Initial request."},
				map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "HISTORY_ANCHOR"}}},
				map[string]any{"type": "message", "role": "user", "content": strings.Repeat("stable historical data ", 10000)},
				map[string]any{"type": "function_call", "call_id": "call_resize", "name": "fixture", "arguments": "{}"},
				map[string]any{"type": "function_call_output", "call_id": "call_resize", "output": "tool result"},
			}
			wire := func(withNotice bool) gjson.Result {
				input := append([]any{}, items...)
				if withNotice {
					input = append(input, map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": notice}}})
				}
				payload, _ := json.Marshal(map[string]any{"model": model, "instructions": "Stable instructions.", "input": input, "tools": []any{map[string]any{"type": "function", "name": "fixture", "parameters": map[string]any{"type": "object"}}}})
				body, err := responsesclaude.ConvertOpenAIResponsesRequestToClaude(model, payload, true)
				if err != nil {
					t.Fatal(err)
				}
				body = checkSystemInstructionsWithSigningModeAt(body, false, false, "2.1.280", "cli", "", "2026-11-02", false, "", "stable-prompt", false, "human")
				body = ensureCacheControl(body)
				body = enforceCacheControlLimit(body, 4)
				body = upgradeClaudeCacheControlTTL(body, "1h")
				return gjson.ParseBytes(body)
			}
			before, after := wire(false), wire(true)
			if before.Get("system").Raw != after.Get("system").Raw {
				t.Fatal("global native system changed")
			}
			oldMessages, newMessages := before.Get("messages").Array(), after.Get("messages").Array()
			if len(newMessages) != len(oldMessages)+1 {
				t.Fatal("notice lost or unrelated history rewritten")
			}
			for i, m := range oldMessages {
				if m.Raw != newMessages[i].Raw {
					t.Fatalf("cache prefix changed at message %d", i)
				}
			}
			last := newMessages[len(newMessages)-1]
			if last.Get("role").String() != "system" || last.Get("content.0.text").String() != notice {
				t.Fatal("notice not preserved at end")
			}
			if err := validateClaudeMidSystemMessageModel([]byte(after.Raw), false, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}
