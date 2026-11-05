package gateway

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	claude "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
	gemini "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini/openai/responses"
	chat "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/openai/openai/responses"
	"github.com/tidwall/gjson"
)

func TestProviderProgressAndFinalMessagePhases(t *testing.T) {
	request := []byte(`{"model":"fixture","tools":[{"type":"function","name":"probe","parameters":{"type":"object"}}]}`)
	for _, mode := range []string{"tool", "final", "incomplete"} {
		claudeStop, chatStop, geminiStop, want := "end_turn", "stop", "STOP", "final_answer"
		if mode == "tool" {
			claudeStop, chatStop, want = "tool_use", "tool_calls", "commentary"
		} else if mode == "incomplete" {
			claudeStop, chatStop, geminiStop, want = "max_tokens", "length", "MAX_TOKENS", "commentary"
		}
		claudeFrames := []string{`{"type":"message_start","message":{"id":"msg_fixture","model":"fixture"}}`, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Verified milestone."}}`, `{"type":"content_block_stop","index":0}`}
		chatFrames := []string{`{"id":"chat_fixture","choices":[{"index":0,"delta":{"content":"Verified milestone."}}]}`}
		geminiFrames := []string{`{"responseId":"fixture","candidates":[{"content":{"parts":[{"text":"Verified milestone."}]}}]}`}
		if mode == "tool" {
			claudeFrames = append(claudeFrames, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_probe","name":"probe","input":{}}}`, `{"type":"content_block_stop","index":1}`)
			chatFrames = append(chatFrames, `{"id":"chat_fixture","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_probe","type":"function","function":{"name":"probe","arguments":"{}"}}]}}]}`)
			geminiFrames = append(geminiFrames, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"probe","args":{}}}]}}]}`)
		}
		claudeFrames = append(claudeFrames, fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q}}`, claudeStop), `{"type":"message_stop"}`)
		chatFrames = append(chatFrames, fmt.Sprintf(`{"id":"chat_fixture","choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, chatStop), `[DONE]`)
		geminiFrames = append(geminiFrames, fmt.Sprintf(`{"candidates":[{"finishReason":%q}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`, geminiStop))
		for _, tc := range []struct {
			name   string
			frames []string
			call   func(context.Context, string, []byte, []byte, []byte, *any) [][]byte
		}{
			{"claude", claudeFrames, claude.ConvertClaudeResponseToOpenAIResponses},
			{"chat", chatFrames, chat.ConvertOpenAIChatCompletionsResponseToOpenAIResponses},
			{"gemini", geminiFrames, gemini.ConvertGeminiResponseToOpenAIResponses},
		} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				var state any
				donePhases := map[string]string{}
				terminal, earlyText := false, false
				for frameIndex, frame := range tc.frames {
					for _, chunk := range tc.call(context.Background(), "fixture", request, request, []byte("data: "+frame), &state) {
						for _, line := range bytes.Split(chunk, []byte("\n")) {
							if !bytes.HasPrefix(line, []byte("data:")) {
								continue
							}
							event := gjson.ParseBytes(bytes.TrimSpace(line[5:]))
							switch event.Get("type").String() {
							case "response.output_text.delta":
								if frameIndex < len(tc.frames)-1 {
									earlyText = true
								}
							case "response.output_item.added":
								if event.Get("item.type").String() == "message" && event.Get("item.phase").String() != "commentary" {
									t.Fatal("in-progress message lacks commentary phase")
								}
							case "response.output_item.done":
								if event.Get("item.type").String() == "message" {
									phase := event.Get("item.phase").String()
									if phase != want {
										t.Fatalf("done phase %q, want %q", phase, want)
									}
									donePhases[event.Get("item.id").String()] = phase
								}
							case "response.completed", "response.incomplete":
								terminal = true
								for _, item := range event.Get("response.output").Array() {
									if item.Get("type").String() == "message" && (item.Get("phase").String() != want || donePhases[item.Get("id").String()] != want) {
										t.Fatal("terminal and item.done phases diverged")
									}
								}
							}
						}
					}
				}
				if !earlyText || !terminal || len(donePhases) != 1 {
					t.Fatalf("missing early text or terminal message: %v %v %v", earlyText, terminal, donePhases)
				}
			})
		}
	}
}
