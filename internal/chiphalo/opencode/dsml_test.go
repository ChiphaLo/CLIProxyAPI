package opencode

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

const execSchema = `{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`

func dsmlInvoke(name, field, value, stringFlag string) string {
	return `<｜｜DSML｜｜ invoke name="` + name + `"><｜｜DSML｜｜ parameter name="` + field + `" string="` + stringFlag + `">` + value + `</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke>`
}
func dsmlRoot(inner string) string { return markers[0] + inner + `</｜｜DSML｜｜ calls>` }

func TestDSMLPreservesJavaScriptAsData(t *testing.T) {
	code := "\n// @exec: {}\nconst x = `a\\nb`;\ntext(x);\n"
	text := dsmlRoot(dsmlInvoke("exec", "input", code, "true"))
	calls, err := RecoverCalls(text, map[string]json.RawMessage{"exec": json.RawMessage(execSchema)}, map[string]bool{"exec": true})
	if err != nil || len(calls) != 1 {
		t.Fatalf("valid envelope rejected: %v", err)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil || args["input"] != code {
		t.Fatal("JavaScript input was converted or rewritten")
	}
	// A valid first call does not authorize returning any part of an invalid batch.
	text = dsmlRoot(dsmlInvoke("exec", "input", code, "true") + dsmlInvoke("unknown", "input", code, "true"))
	calls, err = RecoverCalls(text, map[string]json.RawMessage{"exec": json.RawMessage(execSchema)}, map[string]bool{"exec": true})
	if err == nil || len(calls) != 0 {
		t.Fatal("partial batch escaped validation")
	}
}

func TestDSMLBoundsAndExternalSchemasFailClosed(t *testing.T) {
	valid := dsmlRoot(dsmlInvoke("exec", "input", "text(1);", "true"))
	for _, text := range []string{
		valid + " trailing prose",
		"Example:\n" + valid,
		"```\n" + valid + "\n```",
		valid[:len(valid)-10],
		dsmlRoot(strings.Repeat(dsmlInvoke("exec", "input", "text(1);", "true"), MaxCalls+1)),
		dsmlRoot(dsmlInvoke("exec", "input", strings.Repeat("x", MaxChars), "true")),
		dsmlRoot(dsmlInvoke("exec", "input", valid, "true")),
	} {
		calls, err := RecoverCalls(text, map[string]json.RawMessage{"exec": json.RawMessage(execSchema)}, map[string]bool{"exec": true})
		if err == nil || len(calls) != 0 {
			t.Fatal("malformed/unbounded envelope accepted")
		}
	}
	for _, schema := range []string{`{"$ref":"https://example.invalid/schema"}`, `{"$dynamicRef":"file:///etc/passwd"}`, `{"type":"object","properties":{"input":{"type":"string","$ref":"https://example.invalid/nested"}}}`} {
		calls, err := RecoverCalls(valid, map[string]json.RawMessage{"exec": json.RawMessage(schema)}, map[string]bool{"exec": true})
		if err == nil || len(calls) != 0 {
			t.Fatal("external schema accepted")
		}
	}
}

func TestStrictParameterJSON(t *testing.T) {
	for _, text := range []string{`{"x":1,"x":2}`, `{"x":{"y":1,"y":2}}`, `NaN`, `Infinity`, `{} {}`, `{"x":NaN}`, `1e999999999`, strings.Repeat("9", 4301)} {
		if _, err := strictJSON(text); err == nil {
			t.Fatalf("invalid parameter JSON accepted: %s", text)
		}
	}
	if value, err := strictJSON(`1e-999999999`); err != nil || value != float64(0) {
		t.Fatal("tiny exponent was not normalized before schema validation")
	}
}

func TestDSMLPythonParity(t *testing.T) {
	data, err := os.ReadFile("testdata/dsml-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name         string                     `json:"name"`
		Text         string                     `json:"text"`
		Declarations map[string]json.RawMessage `json:"declarations"`
		Allowed      map[string]bool            `json:"allowed"`
		Accepted     bool                       `json:"accepted"`
		Arguments    []any                      `json:"arguments"`
		Names        []string                   `json:"names"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			calls, err := RecoverCalls(tc.Text, tc.Declarations, tc.Allowed)
			if (err == nil) != tc.Accepted {
				t.Fatalf("Python parity mismatch: accepted=%t / err=%v", tc.Accepted, err)
			}
			if err != nil {
				if len(calls) != 0 {
					t.Fatal("rejected envelope returned partial calls")
				}
				return
			}
			if len(calls) != len(tc.Arguments) {
				t.Fatal("batch length changed")
			}
			for i, call := range calls {
				var args any
				if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || !reflect.DeepEqual(args, tc.Arguments[i]) || call.Function.Name != tc.Names[i] {
					t.Fatal("tool name/arguments changed")
				}
			}
		})
	}
}
