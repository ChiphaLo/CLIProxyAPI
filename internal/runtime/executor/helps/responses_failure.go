package helps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
)

var responsesResetDuration = regexp.MustCompile(`(?i)\b(?:reset|resets|try again)\s+in\s+(?:(\d+)\s*(?:hours?|h)\b\s*)?(?:(\d+)\s*(?:minutes?|mins?|m)\b\s*)?(?:(\d+)\s*(?:seconds?|secs?|s)\b)?`)

// ResponsesStreamFailure retains the upstream error contract without retaining
// the response output, prompt or other fields from its surrounding envelope.
func ResponsesStreamFailure(payload []byte) (int, []byte, *time.Duration) {
	root := ojson.ParseBytes(payload)
	errorNode := root.Get("error")
	if !errorNode.IsObject() {
		errorNode = root.Get("response.error")
	}
	if !errorNode.IsObject() {
		errorNode = root
	}
	status := http.StatusBadGateway
	explicitStatus := false
	for _, path := range []string{"status", "status_code", "error.status", "error.status_code", "response.error.status", "response.error.status_code"} {
		value := root.Get(path)
		if value.Type == ojson.Number && value.Int() >= 400 && value.Int() <= 599 {
			status = int(value.Int())
			explicitStatus = true
			break
		}
	}
	if !explicitStatus {
		switch strings.ToLower(errorNode.Get("type").String()) {
		case "rate_limit_error":
			status = http.StatusTooManyRequests
		case "authentication_error":
			status = http.StatusUnauthorized
		case "permission_error":
			status = http.StatusForbidden
		case "invalid_request_error":
			status = http.StatusBadRequest
		}
		if errorNode.Get("code").String() == "rate_limit_exceeded" {
			status = http.StatusTooManyRequests
		}
		if errorNode.Get("code").String() == "context_length_exceeded" {
			status = http.StatusBadRequest
		}
	}
	message := errorNode.Get("message").String()
	if message == "" {
		message = "Upstream Responses stream reported failure"
	}
	errorType := errorNode.Get("type").String()
	if errorType == "" {
		errorType = "server_error"
	}
	errorBody := map[string]any{"message": message, "type": errorType}
	for _, name := range []string{"code", "param"} {
		if value := errorNode.Get(name); value.Exists() {
			errorBody[name] = value.Value()
		}
	}
	var retryAfter *time.Duration
	if status == http.StatusTooManyRequests {
		seconds := errorNode.Get("retry_after").Int()
		if seconds == 0 {
			seconds = root.Get("retry_after").Int()
		}
		if seconds <= 0 {
			seconds = 0
			if match := responsesResetDuration.FindStringSubmatch(message); match != nil {
				for index, scale := range []int64{3600, 60, 1} {
					value, _ := strconv.ParseInt(match[index+1], 10, 32)
					seconds += value * scale
				}
			}
		}
		// Reject overflowing or implausible duration hints instead of wrapping.
		if seconds > 0 && seconds <= int64((365*24*time.Hour)/time.Second) {
			delay := time.Duration(seconds) * time.Second
			retryAfter = &delay
			errorBody["retry_after"] = seconds
			errorBody["message"] = fmt.Sprintf("%s (try again in %ds)", message, seconds)
		}
	}
	body, _ := json.Marshal(map[string]any{"error": errorBody})
	return status, body, retryAfter
}
