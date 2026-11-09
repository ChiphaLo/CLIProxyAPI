package responses

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

var resizeNoticeLine = regexp.MustCompile(`^Image ([1-9][0-9]{0,6}) of ([1-9][0-9]{0,6}) in the preceding tool output was resized from [1-9][0-9]{0,6}x[1-9][0-9]{0,6} to [1-9][0-9]{0,6}x[1-9][0-9]{0,6} pixels\.$`)

// Only verified modern families accept chronological role=system turns. Keep
// legacy/unknown model behavior, generic developer authority and explicit cache
// placement unchanged. The notification itself is never demoted or discarded.
func responsesInlineImageResizeNotices(model string, input ojson.Result) map[int]string {
	if !input.IsArray() {
		return nil
	}
	return responsesInlineImageResizeNoticesFromItems(model, input.Array())
}
func responsesInlineImageResizeNoticesFromItems(model string, items []ojson.Result) map[int]string {
	model = strings.ToLower(strings.TrimSpace(model))
	model = model[strings.LastIndexByte(model, '/')+1:]
	switch model {
	case "claude-opus-5", "claude-opus-5-5", "claude-sonnet-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5", "claude-fable-5-1":
	default:
		return nil
	}
	result := map[int]string{}
	for i, item := range items {
		if i == 0 || !strings.EqualFold(item.Get("role").String(), "developer") || item.Get("cache_control").Exists() {
			continue
		}
		previous := items[i-1].Get("type").String()
		if previous != "function_call_output" && previous != "custom_tool_call_output" {
			continue
		}
		content := item.Get("content")
		text := ""
		if content.Type == ojson.String {
			text = content.String()
		} else if content.IsArray() && len(content.Array()) == 1 {
			part := content.Array()[0]
			if part.Get("type").String() != "input_text" || part.Get("cache_control").Exists() {
				continue
			}
			text = part.Get("text").String()
		}
		if validImageResizeNotice(text) {
			result[item.Index] = text
		}
	}
	return result
}

func validImageResizeNotice(text string) bool {
	if len(text) > 16384 {
		return false
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n"), "\n")
	if len(lines) < 3 || lines[0] != "<image_resize_notice>" || lines[len(lines)-1] != "</image_resize_notice>" {
		return false
	}
	// Codex lists only resized images; other images keep their original dimensions.
	previousIndex, imageCount := 0, 0
	for _, line := range lines[1 : len(lines)-1] {
		match := resizeNoticeLine.FindStringSubmatch(line)
		if match == nil {
			return false
		}
		index, _ := strconv.Atoi(match[1])
		total, _ := strconv.Atoi(match[2])
		if index <= previousIndex || index > total || (imageCount != 0 && total != imageCount) {
			return false
		}
		previousIndex, imageCount = index, total
	}
	return true
}
