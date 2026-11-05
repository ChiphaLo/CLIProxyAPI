package webgui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

func sortingFixture(t *testing.T) *Server {
	t.Helper()
	s, _ := fixtureServer(t)
	raw := `{"request_id":"a","ts":1200,"model":"gpt-6-astra","key_id":"z","input":"1000","cached":900,"output":10,"duration_ms":2000,"ttft_ms":100,"output_tps":5}
{"request_id":"b","ts":3000,"model":"gpt-6-astra","key_id":"a","input":50,"cached":0,"output":100,"duration_ms":10000,"ttft_ms":4000,"output_tps":10}
{"request_id":"c","ts":2500,"model":"other","key_id":"m","input":100,"cached":10,"output":1,"duration_ms":1000,"ttft_ms":50,"output_tps":1}
{"request_id":"d","ts":2800,"model":"gpt-6-astra","key_id":"b","input":10,"cached":20,"output":0,"duration_ms":0,"ttft_ms":0,"output_tps":999}
{"request_id":"e","ts":1500,"model":"gpt-6-astra","key_id":"c","input":100000,"cached":0,"output":1000,"duration_ms":604800001,"ttft_ms":80,"output_tps":9999}
{"request_id":"outside","ts":6000,"model":"gpt-6-astra","input":999999999}
`
	if err := os.WriteFile(s.options.Usage, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUsageSortingAcrossFilteredRecordsBeforeLimit(t *testing.T) {
	s := sortingFixture(t)
	baseline, err := s.usage(url.Values{"start": {"1000"}, "end": {"5000"}, "limit": {"2"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ field, order, want string }{
		{"ts", "desc", "b,d"}, {"ts", "asc", "a,e"},
		{"model", "desc", "c,b"}, {"model", "asc", "b,d"},
		{"key_id", "desc", "a,c"}, {"key_id", "asc", "b,d"},
		{"input", "desc", "e,a"}, {"input", "asc", "d,b"},
		{"cached", "desc", "a,d"}, {"cached", "asc", "b,e"},
		{"output", "desc", "e,b"}, {"output", "asc", "d,c"},
		{"total", "desc", "e,a"}, {"total", "asc", "d,c"},
		{"cost", "desc", "e,b"}, {"cost", "asc", "c,d"},
		{"duration_ms", "desc", "b,a"}, {"duration_ms", "asc", "c,a"},
		{"ttft_ms", "desc", "b,a"}, {"ttft_ms", "asc", "c,e"},
		{"output_tps", "desc", "b,a"}, {"output_tps", "asc", "c,a"},
	} {
		t.Run(tc.field+"/"+tc.order, func(t *testing.T) {
			query := url.Values{"start": {"1000"}, "end": {"5000"}, "limit": {"2"}, "sort": {tc.field}, "order": {tc.order}}
			value, err := s.usage(query)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, row := range value["recent"].([]map[string]any) {
				ids = append(ids, row["request_id"].(string))
			}
			if strings.Join(ids, ",") != tc.want {
				t.Fatalf("sorted selection = %v; want %s", ids, tc.want)
			}
			for _, key := range []string{"totals", "byModel", "byDay", "byHour", "byBucket", "today", "prices"} {
				if !reflect.DeepEqual(value[key], baseline[key]) {
					t.Fatalf("sorting changed %s", key)
				}
			}
		})
	}
	value, err := s.usage(url.Values{"start": {"1000"}, "end": {"5000"}, "sort": {"cost"}, "order": {"asc"}, "model": {"gpt-6-astra"}})
	if err != nil || value["totals"].(map[string]any)["requests"] != int64(4) || value["recent"].([]map[string]any)[0]["request_id"] != "d" {
		t.Fatal("model filtering must happen before sorted selection")
	}
}

func TestUsageSortingStableTiesAndLivePrices(t *testing.T) {
	s := sortingFixture(t)
	query := url.Values{"start": {"1000"}, "end": {"5000"}, "sort": {"cost"}, "order": {"desc"}, "limit": {"1"}}
	_, _ = s.usage(query)
	var cfg map[string]any
	if readJSON(s.auth.config, 2*1024*1024, true, &cfg) != nil {
		t.Fatal("fixture config unavailable")
	}
	cfg["prices"].(map[string]any)["other"] = map[string]any{"input": 100000}
	if writeJSON(s.auth.config, cfg) != nil {
		t.Fatal("fixture price update failed")
	}
	value, err := s.usage(query)
	if err != nil || value["recent"].([]map[string]any)[0]["request_id"] != "c" {
		t.Fatal("sorting reused stale prices")
	}
	raw := "{\"ts\":2000,\"model\":\"other\",\"request_id\":\"first\"}\n{\"ts\":2000,\"model\":\"other\",\"request_id\":\"second\"}\n"
	if err := os.WriteFile(s.options.Usage, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	for _, order := range []string{"asc", "desc"} {
		query.Set("order", order)
		value, err := s.usage(query)
		if err != nil || value["recent"].([]map[string]any)[0]["request_id"] != "first" {
			t.Fatal("equal values must keep deterministic file order")
		}
	}
}

func TestUsageSortingSelectsOlderRowsBeyondLatestPage(t *testing.T) {
	s, _ := fixtureServer(t)
	var raw strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&raw, "{\"request_id\":\"%d\",\"ts\":%d,\"model\":\"gpt-6-astra\",\"input\":%d}\n", i, 1000+i, 400-i)
	}
	if err := os.WriteFile(s.options.Usage, []byte(raw.String()), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.options.Usage)
	value, err := s.usage(url.Values{"start": {"1000"}, "end": {"5000"}, "sort": {"cost"}, "limit": {"200"}})
	if err != nil {
		t.Fatal(err)
	}
	rows := value["recent"].([]map[string]any)
	if len(rows) != 200 || rows[0]["request_id"] != "0" || rows[199]["request_id"] != "199" || value["totals"].(map[string]any)["requests"] != int64(400) {
		t.Fatal("sort was applied only to the newest page instead of the whole window")
	}
	after, _ := os.ReadFile(s.options.Usage)
	if string(before) != string(after) {
		t.Fatal("sorting changed stored calls")
	}
	field, order := usageSort(url.Values{"sort": {"untrusted"}, "order": {"invalid"}})
	if field != "ts" || order != "desc" {
		t.Fatal("invalid ordering did not fall back to latest-first")
	}
	if _, err := json.Marshal(value); err != nil {
		t.Fatal(err)
	}
}
