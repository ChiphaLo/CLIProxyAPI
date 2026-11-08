package helps

import (
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

var nativeContractIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

// NativeResponsesContractMessage exposes bounded diagnostic identifiers only.
// Tool arguments, request content and arbitrary error strings stay private.
func NativeResponsesContractMessage(err error, event []byte) string {
	reason := "invalid_contract"
	if err != nil {
		switch err.Error() {
		case "undeclared Muse tool call":
			reason = "undeclared_tool"
		case "Muse arguments do not match final declaration", "invalid Muse custom tool envelope", "invalid Muse custom tool input":
			reason = "arguments_schema"
		case "invalid Muse call identity", "conflicting Muse call identity", "conflicting Muse call ID", "Muse call_id is missing", "Muse terminal call arguments conflict":
			reason = "call_identity"
		case "Muse arguments lack a declared call", "Muse call emitted arguments after completion":
			reason = "event_order"
		case "upstream returned an undeclared native custom call type", "upstream emitted a custom event for a function envelope":
			reason = "unexpected_call_type"
		case "Muse tool call limit", "Muse argument limit", "Muse argument buffer limit":
			reason = "contract_limit"
		}
	}
	parts := []string{reason}
	for _, field := range []struct{ label, path string }{{"event", "type"}, {"tool", "item.name"}} {
		if value := gjson.GetBytes(event, field.path).String(); nativeContractIdentifier.MatchString(value) {
			parts = append(parts, field.label+"="+value)
		}
	}
	return "Native Responses tool contract is invalid (" + strings.Join(parts, "; ") + ")"
}
