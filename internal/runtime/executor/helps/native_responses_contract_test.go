package helps

import (
	"errors"
	"strings"
	"testing"
)

func TestNativeResponsesContractMessage(t *testing.T) {
	event := []byte(`{"type":"response.output_item.added","item":{"name":"view_image","arguments":"private-argument-value"}}`)
	for _, test := range []struct{ message, reason string }{
		{"undeclared Muse tool call", "undeclared_tool"},
		{"Muse arguments do not match final declaration", "arguments_schema"},
		{"conflicting Muse call ID", "call_identity"},
		{"Muse arguments lack a declared call", "event_order"},
		{"upstream emitted a custom event for a function envelope", "unexpected_call_type"},
		{"Muse argument limit", "contract_limit"},
		{"arbitrary private error content", "invalid_contract"},
	} {
		message := NativeResponsesContractMessage(errors.New(test.message), event)
		expected := "Native Responses tool contract is invalid (" + test.reason + "; event=response.output_item.added; tool=view_image)"
		if message != expected || strings.Contains(message, "private") {
			t.Fatalf("unexpected diagnostic: %q", message)
		}
	}
	for _, event := range [][]byte{
		[]byte(`{"type":"private\nline","item":{"name":"private\nline"}}`),
		[]byte(`{"type":"` + strings.Repeat("a", 129) + `","item":{"name":"` + strings.Repeat("a", 129) + `"}}`),
	} {
		if message := NativeResponsesContractMessage(nil, event); message != "Native Responses tool contract is invalid (invalid_contract)" {
			t.Fatal("unbounded or control-character identifier was exposed")
		}
	}
}
