package quotafit

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"testing"
)

func compareJSON(t *testing.T, path string, actual, expected any) {
	t.Helper()
	switch e := expected.(type) {
	case nil:
		if actual != nil {
			t.Fatalf("%s: expected null, got %v", path, actual)
		}
	case map[string]any:
		a, ok := actual.(map[string]any)
		if !ok || len(a) != len(e) {
			t.Fatalf("%s: object shape mismatch", path)
		}
		for k, v := range e {
			compareJSON(t, path+"."+k, a[k], v)
		}
	case []any:
		a, ok := actual.([]any)
		if !ok || len(a) != len(e) {
			t.Fatalf("%s: array shape mismatch (%v vs %v)", path, actual, expected)
		}
		for i, v := range e {
			compareJSON(t, path+"[]", a[i], v)
		}
	case float64:
		a, ok := actual.(float64)
		if !ok || math.Abs(a-e) > 2e-7*max(1, math.Abs(e)) {
			t.Fatalf("%s: numeric mismatch %.15g vs %.15g", path, a, e)
		}
	default:
		if actual != expected {
			t.Fatalf("%s: value mismatch %v vs %v", path, actual, expected)
		}
	}
}
func TestPythonFoundationParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/foundations.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name      string          `json:"name"`
		Operation string          `json:"operation"`
		Input     json.RawMessage `json:"input"`
		Expected  json.RawMessage `json:"expected"`
	}
	if json.Unmarshal(raw, &fixtures) != nil {
		t.Fatal("bad synthetic fixtures")
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			var output any
			switch fixture.Operation {
			case "observations", "lag":
				var input struct {
					Prefix    string `json:"prefix"`
					Snapshots []any  `json:"snapshots"`
					Rows      []any  `json:"rows"`
					Config    Config `json:"config"`
					Bucket    int64  `json:"bucket"`
				}
				decoder := json.NewDecoder(bytes.NewReader(fixture.Input))
				decoder.UseNumber()
				if decoder.Decode(&input) != nil {
					t.Fatal("bad input")
				}
				if fixture.Operation == "lag" {
					output = LagProfile(input.Prefix, input.Snapshots, input.Rows, input.Config, nil, input.Bucket)
				} else {
					obs, diag := BuildObservations(input.Prefix, input.Snapshots, input.Rows, input.Config)
					output = map[string]any{"observations": obs, "diagnostics": diag}
				}
			case "numeric":
				var input struct {
					Rows    [][]float64 `json:"rows"`
					Targets []float64   `json:"targets"`
					Weights []float64   `json:"weights"`
					Config  Config      `json:"config"`
					Names   []string    `json:"names"`
				}
				if json.Unmarshal(fixture.Input, &input) != nil {
					t.Fatal("bad numeric input")
				}
				gram, linear := weightedGram(input.Rows, input.Targets, input.Weights)
				theta, weights := solve(input.Rows, input.Targets, input.Weights, configuration(input.Config), nil, nil)
				predictions := []float64{}
				if theta != nil {
					for _, row := range input.Rows {
						predictions = append(predictions, dot(row, theta))
					}
				}
				output = map[string]any{"gram": gram, "linear": linear, "nnls": NNLS(gram, linear, 1e-8, 0), "theta": theta, "weights": weights, "diagnostics": designDiagnostics(input.Rows, input.Weights, input.Names), "metrics": metrics(input.Targets, predictions), "eigenvalues": eigenvalues(gram, 80)}
			default:
				t.Fatal("unknown operation")
			}
			encoded, err := json.Marshal(output)
			if err != nil {
				t.Fatal(err)
			}
			var actual, expected any
			if json.Unmarshal(encoded, &actual) != nil || json.Unmarshal(fixture.Expected, &expected) != nil {
				t.Fatal("invalid result")
			}
			compareJSON(t, fixture.Name, actual, expected)
		})
	}
}
func TestModelAndPercentNormalization(t *testing.T) {
	if NormalizeModel("codex2/gpt-6.1-sol", nil, nil) != "gpt-6.1-sol" || NormalizeModel("provider/unknown", nil, nil) != "provider/unknown" || NormalizeModel("codex/gpt-5.6", nil, nil) != "gpt-5.6-sol" {
		t.Fatal("guessed or lost model alias")
	}
	value, whole := NormalizePercent(.25)
	if value == nil || *value != 25 || whole {
		t.Fatal("raw fraction normalization failed")
	}
	value, whole = NormalizePercent(1)
	if value == nil || *value != 1 || !whole {
		t.Fatal("integer percent incorrectly interpreted as fraction")
	}
}
