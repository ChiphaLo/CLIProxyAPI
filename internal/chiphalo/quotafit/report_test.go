package quotafit

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPythonFullReportParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/report.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name     string
		Input    json.RawMessage
		Expected json.RawMessage
	}
	if json.Unmarshal(raw, &fixtures) != nil {
		t.Fatal("invalid fixtures")
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			var input struct {
				Observations  []Observation
				Config        Config
				Prefix, Label string
				Epoch         int64
			}
			if json.Unmarshal(fixture.Input, &input) != nil {
				t.Fatal("invalid report input")
			}
			report := Fit(input.Observations, input.Config, input.Prefix, input.Label, input.Epoch)
			output := map[string]any{"report": report, "summary": ReportSummary(report)}
			encoded, err := json.Marshal(output)
			if err != nil {
				t.Fatal(err)
			}
			var actual, expected any
			_ = json.Unmarshal(encoded, &actual)
			_ = json.Unmarshal(fixture.Expected, &expected)
			compareJSON(t, fixture.Name, actual, expected)
		})
	}
}
