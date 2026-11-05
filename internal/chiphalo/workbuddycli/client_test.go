package workbuddycli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
)

func fixtureRequest() Request {
	return Request{Model: "deepseek-v4.1-flash", Messages: []Message{{Role: "user", Content: json.RawMessage(`"Call verify_echo once with text violet. Then tell me the exact tool output, not the input argument."`)}}, Tools: []Tool{{Type: "function", Function: Function{Name: "verify_echo", Description: "An inert string fixture. The downstream caller supplies its output.", Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)}}}}
}

func TestPrepareToolHistoryBindsReorderedResults(t *testing.T) {
	r := fixtureRequest()
	r.Messages = append(r.Messages,
		Message{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "a", Type: "function", Function: Function{Name: "verify_echo", Arguments: `{"text":"A"}`}},
			{ID: "b", Type: "function", Function: Function{Name: "verify_echo", Arguments: `{"text":"B"}`}},
		}},
		Message{Role: "tool", ToolCallID: "b", Content: json.RawMessage(`"result B"`)},
		Message{Role: "tool", ToolCallID: "a", Content: json.RawMessage(`"result A"`)})
	p, err := prepare(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.history) != 5 || p.history[2]["output"] != "result A" || p.history[4]["output"] != "result B" {
		t.Fatalf("tool result association changed: %#v", p.history)
	}
}

func TestPrepareRejectsMissingOrDuplicateToolResults(t *testing.T) {
	for _, ids := range [][]string{{"a"}, {"a", "a"}, {"a", "unknown"}} {
		r := fixtureRequest()
		r.Messages = append(r.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "a", Type: "function", Function: Function{Name: "verify_echo", Arguments: `{}`}},
			{ID: "b", Type: "function", Function: Function{Name: "verify_echo", Arguments: `{}`}},
		}})
		for _, id := range ids {
			r.Messages = append(r.Messages, Message{Role: "tool", ToolCallID: id, Content: json.RawMessage(`"fixture"`)})
		}
		if _, err := prepare(r); err == nil {
			t.Fatalf("accepted invalid tool batch: %v", ids)
		}
	}
}

func TestPrepareDoesNotFetchExternalSchemas(t *testing.T) {
	r := fixtureRequest()
	r.Tools[0].Function.Parameters = json.RawMessage(`{"$ref":"http://127.0.0.1:1/private"}`)
	if _, err := prepare(r); err == nil {
		t.Fatal("accepted external schema resolution")
	}
}

func TestPrepareHonorsToolChoiceAndEffort(t *testing.T) {
	r := fixtureRequest()
	r.ToolChoice = json.RawMessage(`"none"`)
	p, err := prepare(r)
	if err != nil || len(p.tools) != 0 || len(p.schemas) != 0 {
		t.Fatalf("tool_choice none ignored: %#v %v", p, err)
	}
	r.ReasoningEffort = "max"
	if _, err = prepare(r); err == nil {
		t.Fatal("silently downgraded unsupported reasoning effort")
	}
	r.ReasoningEffort = "high"
	r.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"undeclared"}}`)
	if _, err = prepare(r); err == nil {
		t.Fatal("accepted undeclared forced tool")
	}
}

func TestPrepareOnlyOmitsKnownMissingReasoningMarker(t *testing.T) {
	r := fixtureRequest()
	r.Messages = append([]Message{{Role: "assistant", Content: json.RawMessage(`"fixture"`), ReasoningContent: "[reasoning unavailable]"}}, r.Messages...)
	if _, err := prepare(r); err != nil {
		t.Fatal(err)
	}
	r.Messages[0].ReasoningContent = "actual external reasoning that must not be lost"
	if _, err := prepare(r); err == nil {
		t.Fatal("silently discarded actual external reasoning")
	}
}

func TestPrepareRejectsUnsupportedModalities(t *testing.T) {
	r := fixtureRequest()
	r.Messages[0].Content = json.RawMessage(`[{"type":"image_url","image_url":{"url":"http://127.0.0.1/private"}}]`)
	if _, err := prepare(r); err == nil {
		t.Fatal("silently dropped image or fetched an arbitrary URL")
	}
}

func TestUsagePreservesExistingCPABillingAndIncludesHiddenReasoningOnce(t *testing.T) {
	raw := json.RawMessage(`{"prompt_tokens":3650,"completion_tokens":127,"total_tokens":3777,"prompt_tokens_details":{"cached_tokens":128},"completion_tokens_details":{"reasoning_tokens":64},"credit":0}`)
	mapped, err := billing.ChatUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	row, err := billing.FromRawUsage(mapped, billing.Row{Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if row.Input != 3650 || row.Cached != 128 || row.Output != 127 {
		t.Fatalf("authoritative usage changed: %+v", row)
	}
	cost := billing.Cost(row, map[string]*billing.Rates{"fixture": {Input: 2, Cached: .2, Output: 10}})
	if cost != .0083396 {
		t.Fatalf("reasoning was added twice or pricing was replaced with provider credits: %.8f", cost)
	}
}

func TestLiveOfficialClient(t *testing.T) {
	if os.Getenv("CPA_WORKBUDDY_LIVE") != "1" {
		t.Skip("explicit isolated Linux acceptance only")
	}
	cfg := Config{Node: "/usr/bin/node", Entry: "/opt/cpa-workbuddy-cli-test-20261017/node_modules/@tencent-ai/codebuddy-code/bin/codebuddy", ProductFile: "/var/lib/cpa-wb-cli-test/product.json", CredentialFile: "/var/lib/cpa-wb-cli-test/access.json", Home: "/var/lib/cpa-wb-cli-test"}
	r := fixtureRequest()
	first, err := cfg.Generate(t.Context(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Calls) != 1 || first.Calls[0].Function.Name != "verify_echo" || first.FinishReason != "tool_calls" {
		t.Fatalf("tool handoff failed: %+v", first)
	}
	var input map[string]string
	if json.Unmarshal([]byte(first.Calls[0].Function.Arguments), &input) != nil || input["text"] != "violet" {
		t.Fatalf("tool input corrupted: %s", first.Calls[0].Function.Arguments)
	}
	t.Logf("tool handoff validated; native operations denied=%d usage=%s", first.DeniedActions, first.Usage)
	r.Messages = append(r.Messages, Message{Role: "assistant", ToolCalls: first.Calls}, Message{Role: "tool", ToolCallID: first.Calls[0].ID, Content: json.RawMessage(`"quartz-actual-tool-result"`)})
	r.Tools = nil
	var deltas strings.Builder
	second, err := cfg.Generate(t.Context(), r, func(s string) error { _, _ = deltas.WriteString(s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Text, "quartz-actual-tool-result") || second.Text != deltas.String() || len(second.Calls) != 0 {
		t.Fatalf("tool output roundtrip or streaming failed: %+v", second)
	}
	t.Logf("actual tool result preserved; streamed bytes=%d usage=%s", deltas.Len(), second.Usage)
}
