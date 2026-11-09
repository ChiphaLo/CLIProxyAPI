package helps

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// ClaudeRequestFacts belongs to one immutable payload generation. Mutations
// invalidate it; executors collect the final facts after payload rules.
type ClaudeRequestFacts struct {
	Get                                                       func(string) ojson.Result
	Has1hTTL, HasAdvisorTool, HasAdvancedTool, HasInlineTools bool
	HasPerTurnTiming, HasClearAt, IsSubagent                  bool
	IsProbeOrHelper                                           bool
}

func CollectClaudeRequestFacts(body []byte) ClaudeRequestFacts {
	get := util.IndexRootFields(util.ParseGJSONBytesNoCopy(body))
	f := ClaudeRequestFacts{Get: get}
	valid := ojson.ValidBytes(body)
	cache := func(block ojson.Result) {
		if valid && block.Get("cache_control.ttl").String() == "1h" {
			f.Has1hTTL = true
		}
	}
	anyTitle, systemTitle := false, false
	title := func(text string, system bool) {
		common := strings.Contains(text, "Return a short title") || strings.Contains(text, "naming a coding session") || strings.Contains(text, "Write the title in the predominant language")
		anyTitle = anyTitle || common || strings.Contains(text, "<session>")
		systemTitle = systemTitle || (system && common)
	}
	tools := get("tools")
	toolCount := 0
	if tools.IsArray() {
		tools.ForEach(func(_, tool ojson.Result) bool {
			toolCount++
			kind := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
			f.HasAdvisorTool = f.HasAdvisorTool || strings.HasPrefix(kind, "advisor_")
			f.HasAdvancedTool = f.HasAdvancedTool || strings.HasPrefix(kind, "tool_search_tool_") || tool.Get("defer_loading").Bool() || tool.Get("input_examples").Exists() || tool.Get("allowed_callers").Exists()
			cache(tool)
			return true
		})
	}
	system := get("system")
	if system.IsArray() {
		system.ForEach(func(_, block ojson.Result) bool { cache(block); title(block.Get("text").String(), true); return true })
	} else {
		title(system.String(), true)
	}
	first := system.Get("0.text").String()
	if system.Type == ojson.String {
		first = system.String()
	}
	user := get("metadata.user_id")
	f.IsSubagent = user.Get("parent_session_id").Exists() || strings.Contains(user.String(), `"parent_session_id"`) || strings.Contains(first, "cc_is_subagent=true")
	f.HasPerTurnTiming = get("output_config.timing").Exists()
	messages := get("messages")
	messageCount := 0
	probeContent := false
	messages.ForEach(func(_, message ojson.Result) bool {
		messageCount++
		lookup := util.IndexRootFields(message)
		role := lookup("role").String()
		f.HasPerTurnTiming = f.HasPerTurnTiming || lookup("output_config.timing").Exists()
		f.HasClearAt = f.HasClearAt || lookup("clear_at").Exists()
		content := lookup("content")
		nonReminder, matched := 0, false
		if content.IsArray() {
			content.ForEach(func(_, block ojson.Result) bool {
				fields := util.IndexRootFields(block)
				if valid && messages.IsArray() && fields("cache_control.ttl").String() == "1h" {
					f.Has1hTTL = true
				}
				f.HasInlineTools = f.HasInlineTools || (strings.EqualFold(strings.TrimSpace(fields("type").String()), "tool_addition") && fields("tool.definition").Exists())
				text := fields("text").String()
				title(text, role == "system")
				t := strings.TrimSpace(text)
				if !strings.Contains(t, "<system-reminder>") {
					nonReminder++
					matched = matched || t == "quota" || t == "test" || t == "." || t == "probe" || (t == "Hi" && fields("cache_control").Exists())
				}
				return true
			})
			probeContent = role == "user" && nonReminder == 1 && matched
		} else {
			text := content.String()
			title(text, role == "system")
			t := strings.TrimSpace(text)
			probeContent = role == "user" && content.Type == ojson.String && (t == "quota" || t == "test" || t == "." || t == "probe")
		}
		return true
	})
	probe := get("max_tokens").Exists() && get("max_tokens").Int() == 1 && toolCount == 0 && (!messages.IsArray() || messageCount == 0 || (messageCount == 1 && probeContent))
	props := get("output_config.format.schema.properties")
	helper := systemTitle
	if props.Exists() {
		helper = props.Get("title").Exists() && len(props.Map()) == 1 && anyTitle
	}
	f.IsProbeOrHelper = probe || helper
	return f
}
