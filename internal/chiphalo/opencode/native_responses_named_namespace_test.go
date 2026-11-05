package opencode

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNativeResponsesNamedNamespaceChoice(t *testing.T) {
	const tools = `"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"probe","description":"PROBE_FREEFORM_OK","parameters":{"type":"object"}}]}]`
	var selected string
	for _, choice := range []string{
		`{"type":"function","namespace":"functions","name":"probe"}`,
		`{"type":"function","name":"functions.probe"}`,
	} {
		raw := []byte(`{"model":"fixture",` + tools + `,"tool_choice":` + choice + `,"input":"hello"}`)
		out, bridge, err := PrepareNativeResponses(raw, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := bridge.BindFinal(out); err != nil {
			t.Fatal(err)
		}
		name := gjson.GetBytes(out, "tool_choice.name").String()
		if name == "" || name != gjson.GetBytes(out, "tools.0.name").String() {
			t.Fatal("named selection did not match the declared wire tool")
		}
		if selected != "" && name != selected {
			t.Fatal("split and qualified names selected different tools")
		}
		selected = name
		if !strings.HasSuffix(gjson.GetBytes(out, "tools.0.description").String(), "PROBE_FREEFORM_OK") {
			t.Fatal("literal tool description changed")
		}
	}
	for _, name := range []string{"functions.missing", "other.probe", "functions.probe.extra"} {
		raw := []byte(`{"model":"fixture",` + tools + `,"tool_choice":{"type":"function","name":"` + name + `"},"input":"hello"}`)
		if _, _, err := PrepareNativeResponses(raw, nil, ""); err == nil {
			t.Fatalf("undeclared tool %q was accepted", name)
		}
	}
}
