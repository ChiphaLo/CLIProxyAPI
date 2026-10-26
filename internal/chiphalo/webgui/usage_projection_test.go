package webgui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
)

func assertUsageProjectionParity(t *testing.T, s *Server) {
	t.Helper()
	var want, got []billing.Row
	if err := scanUsage(s.options.Usage, func(r billing.Row, _ map[string]any) error { want = append(want, r); return nil }); err != nil {
		t.Fatal(err)
	}
	err := s.visitUsage(func(entry usageRecord) error { got = append(got, entry.Row); return nil }, nil)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("projection differs from authoritative decoder: got=%d want=%d err=%v", len(got), len(want), err)
	}
}

func TestUsageProjectionAppendRewriteRotationAndPartialRows(t *testing.T) {
	s, _ := fixtureServer(t)
	line := `{"ts":1791892000,"model":"gpt-6-astra","input":"100","cached":20,"output":5,"request_id":"fixture","unknown":"` + strings.Repeat("x", 500) + `"}` + "\n"
	write := func(raw string) {
		t.Helper()
		if err := os.WriteFile(s.options.Usage, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(strings.Repeat(line, 1200) + "broken\n" + strings.Repeat("x", 100000) + "\n" + `{"ts":1`)
	assertUsageProjectionParity(t, s)
	old := &s.usageCache.blocks[0].rows[0]
	assertUsageProjectionParity(t, s)
	if old != &s.usageCache.blocks[0].rows[0] {
		t.Fatal("unchanged block decoded again")
	}
	f, _ := os.OpenFile(s.options.Usage, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString(`,"model":"late","output":7}` + "\n")
	_ = f.Close()
	assertUsageProjectionParity(t, s)
	if old != &s.usageCache.blocks[0].rows[0] {
		t.Fatal("append invalidated immutable historical blocks")
	}
	// Same length and same inode; no append-only assumptions may conceal edits.
	f, _ = os.OpenFile(s.options.Usage, os.O_WRONLY, 0600)
	_, _ = f.WriteAt([]byte(strings.Replace(line, `"output":5`, `"output":9`, 1)), 0)
	_ = f.Close()
	assertUsageProjectionParity(t, s)
	if s.usageCache.blocks[0].rows[0].Row.Output != 9 {
		t.Fatal("in-place correction hidden by cache")
	}
	write(line)
	assertUsageProjectionParity(t, s)
	if err := os.Rename(s.options.Usage, s.options.Usage+".old"); err != nil {
		t.Fatal(err)
	}
	write(strings.Replace(line, `"output":5`, `"output":8`, 1))
	assertUsageProjectionParity(t, s)
	write("")
	assertUsageProjectionParity(t, s)
}

func TestUsageProjectionRecentFieldsAndPricesStayLive(t *testing.T) {
	s, clock := fixtureServer(t)
	var raw strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&raw, "{\"ts\":%d,\"model\":\"gpt-6-astra\",\"input\":100,\"cached\":20,\"output\":5,\"reasoning\":4,\"extra\":\"keep\"}\n", clock.Unix()-int64(i))
	}
	if err := os.WriteFile(s.options.Usage, []byte(raw.String()), 0600); err != nil {
		t.Fatal(err)
	}
	query := url.Values{"limit": {"20"}}
	first, err := s.usage(query)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.usage(query)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("warm projection changed statistics")
	}
	recent := second["recent"].([]map[string]any)
	if len(recent) != 20 || recent[0]["extra"] != "keep" || recent[0]["reasoning"] != float64(4) || recent[0]["input"] != int64(80) {
		t.Fatal("recent fields or token interpretation changed")
	}
	var prices map[string]any
	if readJSON(s.auth.config, 2*1024*1024, true, &prices) != nil {
		t.Fatal("fixture prices missing")
	}
	prices["prices"] = map[string]any{"gpt-6-astra": map[string]any{"input": 20, "cached": 2, "output": 100}}
	if writeJSON(s.auth.config, prices) != nil {
		t.Fatal("price update failed")
	}
	updated, err := s.usage(query)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first["totals"])
	b, _ := json.Marshal(updated["totals"])
	if string(a) == string(b) {
		t.Fatal("cached projection froze prices")
	}
}

func TestUsageProjectionCapacityDoesNotDropRows(t *testing.T) {
	s, _ := fixtureServer(t)
	line := `{"ts":1,"model":"` + strings.Repeat("m", 1024) + `","input":1}` + "\n"
	count := usageCacheBytes/projectionBytes(billing.Row{Model: strings.Repeat("m", 1024)}) + 10
	if err := os.WriteFile(s.options.Usage, []byte(strings.Repeat(line, count)), 0600); err != nil {
		t.Fatal(err)
	}
	seen := 0
	if err := s.visitUsage(func(usageRecord) error { seen++; return nil }, nil); err != nil || seen != count || len(s.usageCache.blocks) == 0 {
		t.Fatalf("capacity dropped rows or discarded the bounded cache: seen=%d count=%d err=%v", seen, count, err)
	}
	bytes, rows := 0, 0
	for _, block := range s.usageCache.blocks {
		bytes += block.bytes
		rows += len(block.rows)
	}
	if bytes > usageCacheBytes || rows > usageCacheRows || rows >= count {
		t.Fatalf("cache budget not enforced: bytes=%d rows=%d count=%d", bytes, rows, count)
	}
	old := &s.usageCache.blocks[0].rows[0]
	assertUsageProjectionParity(t, s)
	if old != &s.usageCache.blocks[0].rows[0] {
		t.Fatal("overflow discarded verified historical blocks on the next poll")
	}
	// Both cached and uncached corrections must remain visible at capacity.
	f, err := os.OpenFile(s.options.Usage, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(line, `"input":1`, `"input":7`, 1)
	_, firstErr := f.WriteAt([]byte(changed), 0)
	_, lastErr := f.WriteAt([]byte(changed), int64((count-1)*len(line)))
	closeErr := f.Close()
	if firstErr != nil || lastErr != nil || closeErr != nil {
		t.Fatal("cannot correct overflow fixture")
	}
	assertUsageProjectionParity(t, s)
	if s.usageCache.blocks[0].rows[0].Row.Input != 7 {
		t.Fatal("cached correction hidden at capacity")
	}
}

func TestUsageProjectionRowCapacityRetainsBoundedCache(t *testing.T) {
	s, _ := fixtureServer(t)
	line := "{\"ts\":1}\n"
	count := usageCacheRows + 1000
	if err := os.WriteFile(s.options.Usage, []byte(strings.Repeat(line, count)), 0600); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		seen := 0
		if err := s.visitUsage(func(usageRecord) error { seen++; return nil }, nil); err != nil || seen != count {
			t.Fatalf("row capacity lost records: %d, %v", seen, err)
		}
		rows := 0
		for _, block := range s.usageCache.blocks {
			rows += len(block.rows)
		}
		if rows <= 0 || rows > usageCacheRows {
			t.Fatalf("row budget not enforced: %d", rows)
		}
	}
}

func BenchmarkUsageProjectionOverflow(b *testing.B) {
	s := &Server{options: Options{Usage: b.TempDir() + "/usage.jsonl", Now: time.Now}}
	line := `{"ts":1791892000,"model":"opencode/deepseek-v4.1-flash","routed_model":"opencode/deepseek-v4.1-flash","key_id":"fixture","input":1000,"cached":900,"output":20,"request_id":"fixture-request","upstream_reported_model":"deepseek-v4.1-flash","path":"/v1/responses"}` + "\n"
	if err := os.WriteFile(s.options.Usage, []byte(strings.Repeat(line, 110000)), 0600); err != nil {
		b.Fatal(err)
	}
	visit := func(usageRecord) error { return nil }
	if err := s.visitUsage(visit, nil); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.visitUsage(visit, nil); err != nil {
			b.Fatal(err)
		}
	}
}
