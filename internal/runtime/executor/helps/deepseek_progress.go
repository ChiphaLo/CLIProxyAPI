package helps

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const deepSeekMilestoneGuidance = `<cpa-progress-policy version="1">
Work quietly between meaningful milestones. Do not narrate routine reads, searches, tool calls, retries, or internal deliberations. Give a user-facing progress update only when a verifiable stage finishes, a material blocker or user decision arises, an important risk needs disclosure, or the task is complete. Keep each update to 1-3 short sentences about actual outcomes, without repeated plans or status-only chatter. Very short tasks may use only the final answer. Preserve necessary safety notices, permission requests, tool execution, and final explanations. This controls commentary cadence only: do not reduce reasoning, skip work or verification, change the user's task, or disregard the requested answer format.
</cpa-progress-policy>`

// applyDeepSeekMilestoneGuidance is request-side steering, not response filtering.
// Match the routed model, not its public alias, and run before user payload rules.
func applyDeepSeekMilestoneGuidance(payload []byte, model, protocol, root string) []byte {
	name := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	name = name[strings.LastIndex(name, "/")+1:]
	if name != "deepseek" && !strings.HasPrefix(name, "deepseek-") {
		return payload
	}
	if !gjson.ValidBytes(payload) || !gjson.ParseBytes(payload).IsObject() {
		return payload
	}
	switch strings.ToLower(protocol) {
	case "openai", "chat", "chat-completions":
		path := buildPayloadPath(root, "messages")
		messages := gjson.GetBytes(payload, path)
		if !messages.IsArray() {
			return payload
		}
		items := messages.Array()
		for i, message := range items {
			if message.Get("role").String() == "system" {
				return appendMilestoneContent(payload, path+"."+strconv.Itoa(i)+".content")
			}
		}
		system, _ := json.Marshal(map[string]string{"role": "system", "content": deepSeekMilestoneGuidance})
		body := []byte("[" + string(system))
		if len(items) != 0 {
			body = append(body, ',')
		}
		body = append(body, messages.Raw[1:]...)
		if out, err := sjson.SetRawBytes(payload, path, body); err == nil {
			return out
		}
	case "openai-response", "responses":
		return appendMilestoneContent(payload, buildPayloadPath(root, "instructions"))
	case "claude":
		return appendMilestoneContent(payload, buildPayloadPath(root, "system"))
	case "gemini":
		key := "systemInstruction"
		if gjson.GetBytes(payload, buildPayloadPath(root, "system_instruction")).Exists() {
			key = "system_instruction"
		}
		path := buildPayloadPath(root, key+".parts")
		parts := gjson.GetBytes(payload, path)
		if parts.Exists() && !parts.IsArray() {
			return payload
		}
		for _, part := range parts.Array() {
			if strings.Contains(part.Get("text").String(), deepSeekMilestoneGuidance) {
				return payload
			}
		}
		if out, err := sjson.SetBytes(payload, path+".-1", map[string]string{"text": deepSeekMilestoneGuidance}); err == nil {
			return out
		}
	}
	return payload
}

func appendMilestoneContent(payload []byte, path string) []byte {
	content := gjson.GetBytes(payload, path)
	if !content.Exists() || content.Type == gjson.Null || content.Type == gjson.String {
		text := content.String()
		if strings.Contains(text, deepSeekMilestoneGuidance) {
			return payload
		}
		if text != "" {
			text += "\n\n"
		}
		if out, err := sjson.SetBytes(payload, path, text+deepSeekMilestoneGuidance); err == nil {
			return out
		}
	} else if content.IsArray() {
		for _, part := range content.Array() {
			if strings.Contains(part.Get("text").String(), deepSeekMilestoneGuidance) {
				return payload
			}
		}
		if out, err := sjson.SetBytes(payload, path+".-1", map[string]string{"type": "text", "text": deepSeekMilestoneGuidance}); err == nil {
			return out
		}
	}
	return payload
}
