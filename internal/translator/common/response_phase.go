package common

import (
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// SetResponseMessagePhases classifies synthesized JSON responses after their
// output order and client tool calls are known. Native Responses bypass it.
func SetResponseMessagePhases(body []byte) []byte {
	items := gjson.GetBytes(body, "output").Array()
	lastMessage := -1
	final := gjson.GetBytes(body, "status").String() == "completed"
	for i, item := range items {
		switch item.Get("type").String() {
		case "message":
			lastMessage = i
		case "function_call", "custom_tool_call":
			final = false
		}
	}
	for i, item := range items {
		if item.Get("type").String() != "message" {
			continue
		}
		phase := "commentary"
		if final && i == lastMessage {
			phase = "final_answer"
		}
		body, _ = sjson.SetBytes(body, "output."+strconv.Itoa(i)+".phase", phase)
	}
	return body
}
