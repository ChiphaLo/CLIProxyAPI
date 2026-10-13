package quotafit

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPythonStructureAndBootstrapParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/structure.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name      string
		Operation string
		Input     json.RawMessage
		Expected  json.RawMessage
	}
	if json.Unmarshal(raw, &fixtures) != nil {
		t.Fatal("invalid fixtures")
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			var output any
			if fixture.Operation == "rng" {
				var input struct {
					Seed int64
					N    int
				}
				if json.Unmarshal(fixture.Input, &input) != nil {
					t.Fatal("invalid RNG input")
				}
				r := newPythonRandom(input.Seed)
				values := []int{}
				for i := 0; i < 40; i++ {
					values = append(values, r.below(input.N))
				}
				output = values
			} else {
				var input struct {
					Observations []Observation
					Config       Config
				}
				if json.Unmarshal(fixture.Input, &input) != nil {
					t.Fatal("invalid structure input")
				}
				cfg := configuration(input.Config)
				s := structure(input.Observations, cfg)
				fit := fitStructure(input.Observations, s, cfg, nil, nil, nil)
				var q any
				var reference *string
				boot := map[string]BootstrapEntry{}
				if fit != nil {
					values := quantities(fit, s)
					q = values
					reference = referenceModel(s, values, s.Active)
					boot = bootstrap(fit, s, input.Observations, cfg)
				}
				output = map[string]any{"structure": s, "fit": fit, "quantities": q, "reference": reference, "bootstrap": boot}
			}
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
