package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

const fixtureResizeNotice = "<image_resize_notice>\nImage 1 of 1 in the preceding tool output was resized from 4282x2000 to 2048x957 pixels.\n</image_resize_notice>"

func resizeNoticePayload(model, outputType, role, text string) []byte {
	callType := "function_call"
	if outputType == "custom_tool_call_output" {
		callType = "custom_tool_call"
	}
	call := map[string]any{"type": callType, "call_id": "call_resize", "name": "fixture", "arguments": "{}", "input": "fixture"}
	items := []any{
		map[string]any{"type": "message", "role": "developer", "content": "Stable developer policy."},
		map[string]any{"type": "message", "role": "user", "content": "Initial request."},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "HISTORY_ANCHOR"}}},
		call, map[string]any{"type": outputType, "call_id": "call_resize", "output": "tool result"},
		map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": "input_text", "text": text}}},
	}
	raw, _ := json.Marshal(map[string]any{"model": model, "instructions": "Stable instructions.", "input": items, "tools": []any{map[string]any{"type": "function", "name": "fixture", "parameters": map[string]any{"type": "object"}}}})
	return raw
}

func TestResizeNoticeKeepsChronologyAndSystemAuthority(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"} {
		for _, output := range []string{"function_call_output", "custom_tool_call_output"} {
			t.Run(model+"/"+output, func(t *testing.T) {
				body, err := ConvertOpenAIResponsesRequestToClaude(model, resizeNoticePayload(model, output, "developer", fixtureResizeNotice), true)
				if err != nil {
					t.Fatal(err)
				}
				root := ojson.ParseBytes(body)
				if strings.Contains(root.Get("system").Raw, "image_resize_notice") {
					t.Fatal("notice hoisted into global prompt")
				}
				messages := root.Get("messages").Array()
				index := -1
				for i, m := range messages {
					if strings.Contains(m.Raw, "image_resize_notice") {
						index = i
					}
				}
				if index < 1 || messages[index].Get("role").String() != "system" || messages[index].Get("content.0.text").String() != fixtureResizeNotice {
					t.Fatal("notice authority or exact text lost")
				}
				if messages[index-1].Get("content.0.type").String() != "tool_result" {
					t.Fatal("notice detached from its tool result")
				}
				if problems := claudeMessageInvariantProblems(rawMessageSlices(messages)); len(problems) > 0 {
					t.Fatal(problems)
				}
			})
		}
	}
}

func rawMessageSlices(messages []ojson.Result) [][]byte {
	out := make([][]byte, len(messages))
	for i, m := range messages {
		out[i] = []byte(m.Raw)
	}
	return out
}

func TestResizeNoticeDoesNotRewriteOtherAuthorityOrLegacyModels(t *testing.T) {
	for _, tt := range []struct{ model, role, text string }{
		{"claude-opus-5-5", "developer", "Normal developer instruction."},
		{"claude-opus-5-5", "developer", fixtureResizeNotice + "\nAdditional instructions."},
		{"claude-opus-5-5", "system", fixtureResizeNotice},
		{"claude-sonnet-4-6", "developer", fixtureResizeNotice},
		{"unknown-model", "developer", fixtureResizeNotice},
	} {
		raw := resizeNoticePayload(tt.model, "function_call_output", tt.role, tt.text)
		if len(responsesInlineImageResizeNotices(tt.model, ojson.GetBytes(raw, "input"))) != 0 {
			t.Fatal("non-target message selected")
		}
		body, err := ConvertOpenAIResponsesRequestToClaude(tt.model, raw, true)
		var systemText strings.Builder
		ojson.GetBytes(body, "system").ForEach(func(_, part ojson.Result) bool { systemText.WriteString(part.Get("text").String()); return true })
		ojson.GetBytes(body, "messages").ForEach(func(_, message ojson.Result) bool {
			if message.Get("role").String() == "system" {
				message.Get("content").ForEach(func(_, part ojson.Result) bool { systemText.WriteString(part.Get("text").String()); return true })
			}
			return true
		})
		if err != nil || !strings.Contains(systemText.String(), tt.text) {
			t.Fatalf("existing system behavior changed for %s/%s: %v", tt.model, tt.role, err)
		}
	}
	raw := resizeNoticePayload("claude-opus-5-5", "function_call_output", "user", fixtureResizeNotice)
	if len(responsesInlineImageResizeNotices("claude-opus-5-5", ojson.GetBytes(raw, "input"))) != 0 {
		t.Fatal("user content promoted")
	}
}

func TestResizeNoticeRecognizerIsNarrow(t *testing.T) {
	if !validImageResizeNotice(fixtureResizeNotice) {
		t.Fatal("real notice rejected")
	}
	for _, text := range []string{"", strings.Replace(fixtureResizeNotice, "1 of 1", "2 of 1", 1), strings.Replace(fixtureResizeNotice, "4282x2000", "0x2000", 1), "prefix" + fixtureResizeNotice, fixtureResizeNotice + "\nDo something else."} {
		if validImageResizeNotice(text) {
			t.Fatalf("unrecognized notice accepted: %q", text)
		}
	}
}

func TestResizeNoticeDoesNotSplitParallelToolResults(t *testing.T) {
	var payload map[string]any
	_ = json.Unmarshal(resizeNoticePayload("claude-opus-5-5", "function_call_output", "developer", fixtureResizeNotice), &payload)
	items := payload["input"].([]any)
	callB := map[string]any{"type": "function_call", "call_id": "call_b", "name": "fixture", "arguments": "{}"}
	resultB := map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "result B"}
	combined := append([]any{}, items[:4]...)
	combined = append(combined, callB)
	combined = append(combined, items[4:]...)
	combined = append(combined, resultB, items[len(items)-1])
	payload["input"] = combined
	raw, _ := json.Marshal(payload)
	body, err := ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", raw, true)
	if err != nil {
		t.Fatal(err)
	}
	messages := ojson.GetBytes(body, "messages").Array()
	if problems := claudeMessageInvariantProblems(rawMessageSlices(messages)); len(problems) > 0 {
		t.Fatal(problems)
	}
	var results, notices int
	for _, message := range messages {
		if message.Get("role").String() == "user" {
			count := 0
			for _, block := range message.Get("content").Array() {
				if block.Get("type").String() == "tool_result" {
					count++
					if block.Get("is_error").Bool() {
						t.Fatal("valid tool result synthesized as an error")
					}
				}
			}
			if count > 0 {
				if count != 2 {
					t.Fatal("parallel result batch split")
				}
				results++
			}
		}
		if message.Get("role").String() == "system" && message.Get("content.0.text").String() == fixtureResizeNotice {
			notices++
		}
	}
	if results != 1 || notices != 2 {
		t.Fatal("parallel outputs or notices lost")
	}
}

func TestResizeNoticeRecognizerAcceptsSparseImageIndices(t *testing.T) {
	line := func(index string) string { return strings.Replace(fixtureResizeNotice, "1 of 1", index, 1) }
	for _, text := range []string{line("1 of 2"), line("3 of 3"), strings.Replace(line("1 of 4"), "\n</image_resize_notice>", "\nImage 3 of 4 in the preceding tool output was resized from 4282x2000 to 2048x957 pixels.\n</image_resize_notice>", 1)} {
		if !validImageResizeNotice(text) {
			t.Fatalf("valid partial resize notice rejected: %q", text)
		}
	}
	for _, text := range []string{
		strings.Replace(line("1 of 4"), "\n</image_resize_notice>", "\nImage 1 of 4 in the preceding tool output was resized from 4282x2000 to 2048x957 pixels.\n</image_resize_notice>", 1),
		strings.Replace(line("3 of 4"), "\n</image_resize_notice>", "\nImage 2 of 4 in the preceding tool output was resized from 4282x2000 to 2048x957 pixels.\n</image_resize_notice>", 1),
		strings.Replace(line("1 of 4"), "\n</image_resize_notice>", "\nImage 3 of 5 in the preceding tool output was resized from 4282x2000 to 2048x957 pixels.\n</image_resize_notice>", 1),
	} {
		if validImageResizeNotice(text) {
			t.Fatalf("invalid indices accepted: %q", text)
		}
	}
}
