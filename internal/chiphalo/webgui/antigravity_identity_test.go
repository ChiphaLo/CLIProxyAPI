package webgui

import (
	"fmt"
	"net/url"
	"os"
	"testing"
)

func TestAntigravityRecentCallsUseReportedIDWithoutRewritingHistory(t *testing.T) {
	s, clock := fixtureServer(t)
	data := []byte(fmt.Sprintf(`{"ts":%d,"model":"antigravity/gemini-3.8-flash-high","routed_model":"gemini-3.8-flash-high","upstream_reported_model":"gemini-3.8-flash-n","model_mismatch":true,"input":100,"output":10,"cached":20}`+"\n", clock.Unix()-1))
	if err := os.WriteFile(s.options.Usage, data, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		value, err := s.usage(url.Values{})
		if err != nil {
			t.Fatal(err)
		}
		row := value["recent"].([]map[string]any)[0]
		if row["display_model"] != "antigravity/gemini-3.8-flash-n" || row["model"] != "antigravity/gemini-3.8-flash-high" || row["model_mismatch"] != false || row["input"] != int64(80) || row["output"] != float64(10) {
			t.Fatalf("wrong read-only projection: %+v", row)
		}
	}
	after, err := os.ReadFile(s.options.Usage)
	if err != nil || string(after) != string(data) {
		t.Fatal("historical receipt rewritten")
	}
}
