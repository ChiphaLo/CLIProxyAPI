package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

func TestClaudeRequestFactsPreserveLegacyClassifiers(t *testing.T) {
	for i := 0; i < 256; i++ {
		d := ojson.NewDoc([]byte(`{"model":"claude-opus-5-5","max_tokens":64,"system":[{"type":"text","text":"rules"}],"messages":[{"role":"user","content":[{"type":"text","text":"question"}]}],"tools":[]}`))
		if i&1 != 0 {
			_ = d.Set("max_tokens", 1)
			_ = d.Set("messages.0.content.0.text", "probe")
		}
		if i&2 != 0 {
			_ = d.Set("system.0.text", "Return a short title")
		}
		if i&4 != 0 {
			_ = d.Set("messages.0.content.0.cache_control.ttl", "1h")
		}
		if i&8 != 0 {
			_ = d.Set("tools.0.type", "advisor_20260101")
		}
		if i&16 != 0 {
			_ = d.Set("tools.1.defer_loading", true)
		}
		if i&32 != 0 {
			_ = d.Set("messages.0.content.1.type", "tool_addition")
			_ = d.SetRaw("messages.0.content.1.tool.definition", `{}`)
		}
		if i&64 != 0 {
			_ = d.Set("messages.0.output_config.timing", "enabled")
			_ = d.Set("messages.0.clear_at", "turn")
		}
		if i&128 != 0 {
			_ = d.SetRaw("output_config.format.schema.properties", `{"title":{"type":"string"}}`)
			_ = d.Set("messages.0.content.0.text", "<session>")
		}
		body := d.Bytes()
		facts := helps.CollectClaudeRequestFacts(body)
		checks := map[string]bool{
			"probe":    facts.IsProbeOrHelper == helps.IsClaudeProbeOrHelperRequest(body),
			"ttl":      facts.Has1hTTL == helps.ClaudePayloadHas1hTTL(body),
			"advisor":  facts.HasAdvisorTool == claudeBodyHasAdvisorTool(body),
			"advanced": facts.HasAdvancedTool == claudeBodyUsesAdvancedToolUse(body),
			"inline":   facts.HasInlineTools == claudeIncludeInlineTools(body, nil),
			"timing":   facts.HasPerTurnTiming == claudeIncludePerTurnTiming(body, nil),
			"subagent": facts.IsSubagent == helps.IsClaudeSubagentRequest(nil, body),
		}
		for name, ok := range checks {
			if !ok {
				t.Fatal(fmt.Sprintf("case %d %s classifier changed", i, name))
			}
		}
	}
}

func TestCachedClaudeCachePathsPreserveMutations(t *testing.T) {
	body := []byte(`{"tools":[{"name":"a","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	for _, strip := range []bool{false, true} {
		a, b := ojson.NewDoc(body), ojson.NewDoc(body)
		paths := collectClaudeCacheControlPathsDoc(b)
		enforceCacheControlLimitDoc(a, 4)
		enforceCacheControlLimitDoc(b, 4, paths)
		if strip {
			stripClaudeCacheControlTTLDoc(a)
			stripClaudeCacheControlTTLDoc(b, paths)
		} else {
			upgradeClaudeCacheControlTTLDoc(a, "1h")
			upgradeClaudeCacheControlTTLDoc(b, "1h", paths)
		}
		normalizeCacheControlTTLDoc(a)
		normalizeCacheControlTTLDoc(b, paths)
		if string(a.Bytes()) != string(b.Bytes()) {
			t.Fatal("cached cache-control paths changed prefix or TTL order")
		}
	}
}

func TestFactsLargeMediaDoesNotBecomeHelper(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"` + strings.Repeat("ABCD", 1<<16) + `"}}]}]}`)
	if helps.CollectClaudeRequestFacts(body).IsProbeOrHelper {
		t.Fatal("media classified as helper")
	}
}
