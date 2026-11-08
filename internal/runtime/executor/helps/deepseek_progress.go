package helps

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
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
	return applyMilestoneGuidance(payload, protocol, root)
}

// applySWEMilestoneGuidance steers SWE models without touching other routed
// models, even when a public alias happens to contain SWE. Text and tool calls
// are never suppressed on the response path.
func applySWEMilestoneGuidance(payload []byte, model, protocol, root string) []byte {
	name := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	name = name[strings.LastIndex(name, "/")+1:]
	if name != "swe" && !strings.HasPrefix(name, "swe-") {
		return payload
	}
	if protocol == "devin" && ojson.ValidBytes(payload) && ojson.ParseBytes(payload).IsObject() {
		return appendMilestoneContent(payload, buildPayloadPath(root, "system_prompt"))
	}
	return applyMilestoneGuidance(payload, protocol, root)
}

func applyMilestoneGuidance(payload []byte, protocol, root string) []byte {
	doc := ojson.NewDoc(payload)
	if applyMilestoneGuidanceDoc(doc, protocol, root) {
		return doc.Bytes()
	}
	return payload
}

func applyMilestoneGuidanceDoc(doc *ojson.Doc, protocol, root string) bool {
	switch strings.ToLower(protocol) {
	case "openai", "chat", "chat-completions":
		path := buildPayloadPath(root, "messages")
		messages := doc.Get(path)
		if !messages.IsArray() {
			return false
		}
		items := messages.Array()
		for i, message := range items {
			if message.Get("role").String() == "system" {
				return appendMilestoneContentDoc(doc, path+"."+strconv.Itoa(i)+".content")
			}
		}
		system, _ := json.Marshal(map[string]string{"role": "system", "content": deepSeekMilestoneGuidance})
		body := []byte("[" + string(system))
		if len(items) != 0 {
			body = append(body, ',')
		}
		body = append(body, messages.Raw[1:]...)
		return doc.SetRawBytes(path, body) == nil
	case "openai-response", "responses":
		return appendMilestoneContentDoc(doc, buildPayloadPath(root, "instructions"))
	case "claude":
		return appendMilestoneContentDoc(doc, buildPayloadPath(root, "system"))
	case "gemini":
		key := "systemInstruction"
		if doc.Get(buildPayloadPath(root, "system_instruction")).Exists() {
			key = "system_instruction"
		}
		path := buildPayloadPath(root, key+".parts")
		parts := doc.Get(path)
		if parts.Exists() && !parts.IsArray() {
			return false
		}
		for _, part := range parts.Array() {
			if strings.Contains(part.Get("text").String(), deepSeekMilestoneGuidance) {
				return false
			}
		}
		return doc.Set(path+".-1", map[string]string{"text": deepSeekMilestoneGuidance}) == nil
	}
	return false
}

func appendMilestoneContent(payload []byte, path string) []byte {
	doc := ojson.NewDoc(payload)
	if appendMilestoneContentDoc(doc, path) {
		return doc.Bytes()
	}
	return payload
}

func appendMilestoneContentDoc(doc *ojson.Doc, path string) bool {
	content := doc.Get(path)
	if !content.Exists() || content.Type == ojson.Null || content.Type == ojson.String {
		text := content.String()
		if strings.Contains(text, deepSeekMilestoneGuidance) {
			return false
		}
		if text != "" {
			text += "\n\n"
		}
		return doc.Set(path, text+deepSeekMilestoneGuidance) == nil
	}
	if content.IsArray() {
		for _, part := range content.Array() {
			if strings.Contains(part.Get("text").String(), deepSeekMilestoneGuidance) {
				return false
			}
		}
		return doc.Set(path+".-1", map[string]string{"type": "text", "text": deepSeekMilestoneGuidance}) == nil
	}
	return false
}
