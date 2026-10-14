package webgui

import (
	"bufio"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func compareFitJSON(t *testing.T, path string, actual, expected any) {
	t.Helper()
	switch e := expected.(type) {
	case nil:
		if actual != nil {
			t.Fatalf("%s: expected null", path)
		}
	case map[string]any:
		a, ok := actual.(map[string]any)
		if !ok || len(a) != len(e) {
			t.Fatalf("%s: object shape mismatch (%d vs %d)", path, len(a), len(e))
		}
		for key, value := range e {
			compareFitJSON(t, path+"."+key, a[key], value)
		}
	case []any:
		a, ok := actual.([]any)
		if !ok || len(a) != len(e) {
			t.Fatalf("%s: array shape mismatch", path)
		}
		for i, value := range e {
			compareFitJSON(t, path+"[]", a[i], value)
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
func TestPythonQuotaWrapperParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/quota-wrapper.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string
		Input struct {
			Window                         string
			Cycles                         int
			History, Usage                 []json.RawMessage
			Quota, Exposed, Prices, Config json.RawMessage
			Epoch                          int64
		}
		Expected json.RawMessage
	}
	if json.Unmarshal(raw, &cases) != nil {
		t.Fatal("bad fixtures")
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			s, clock := fixtureServer(t)
			*clock = time.Unix(c.Input.Epoch, 0)
			write := func(path string, raw []byte) {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(s.options.Quota, c.Input.Quota)
			write(s.options.Exposed, c.Input.Exposed)
			write(filepath.Join(s.options.Data, "quota-model.json"), c.Input.Config)
			var config map[string]json.RawMessage
			_ = readJSON(s.auth.config, 2*1024*1024, true, &config)
			config["prices"] = c.Input.Prices
			if err := writeJSON(s.auth.config, config); err != nil {
				t.Fatal(err)
			}
			writeLines := func(path string, rows []json.RawMessage) {
				file, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				w := bufio.NewWriter(file)
				for _, row := range rows {
					_, _ = w.Write(row)
					_ = w.WriteByte('\n')
				}
				_ = w.Flush()
				_ = file.Close()
				_ = os.Chmod(path, 0600)
			}
			s.options.History = filepath.Join(s.options.Data, "history.jsonl")
			writeLines(s.options.History, c.Input.History)
			writeLines(s.options.Usage, c.Input.Usage)
			result, err := s.computeQuotaFit(c.Input.Window, c.Input.Cycles)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var actual, expected any
			_ = json.Unmarshal(encoded, &actual)
			_ = json.Unmarshal(c.Expected, &expected)
			compareFitJSON(t, c.Name, actual, expected)
		})
	}
}
func waitFitWorker(t *testing.T, s *Server) {
	t.Helper()
	s.fitCache.mu.Lock()
	done := s.fitCache.done
	s.fitCache.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finish")
	}
}
func TestQuotaFitCacheSingleFlightAndViewIsolation(t *testing.T) {
	s, _ := fixtureServer(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.fitCache.compute = func(window string, cycles int) (map[string]any, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return map[string]any{"status": "stable", "computing": false, "window": window, "cycles": cycles}, nil
	}
	value := s.quotaFitReport("recent", 6, false)
	if value["computing"] != true {
		t.Fatal("cold request was blocking")
	}
	<-entered
	for i := 0; i < 8; i++ {
		value = s.quotaFitReport("all", 2, false)
		if value["window"] == "recent" {
			t.Fatal("wrong-view data returned")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("parallel computation started")
	}
	close(release)
	waitFitWorker(t, s)
	if value = s.quotaFitReport("recent", 6, false); value["computing"] != false || calls.Load() != 1 {
		t.Fatal("fresh cache not reused")
	}
	value = s.quotaFitReport("all", 2, false)
	if value["computing"] != true || value["window"] == "recent" {
		t.Fatal("view selection did not invalidate")
	}
	waitFitWorker(t, s)
	if value = s.quotaFitReport("all", 2, false); value["window"] != "all" || value["cycles"] != 2 {
		t.Fatal("selected view not published")
	}
}
func TestQuotaFitSourceChangesAndTTLRefresh(t *testing.T) {
	s, clock := fixtureServer(t)
	var calls atomic.Int32
	s.fitCache.compute = func(string, int) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"status": "stable", "computing": false}, nil
	}
	_ = s.quotaFitReport("recent", 6, false)
	waitFitWorker(t, s)
	_ = s.quotaFitReport("recent", 6, false)
	if calls.Load() != 1 {
		t.Fatal("cache not reused")
	}
	file, err := os.OpenFile(s.options.Usage, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("{}\n")
	_ = file.Close()
	if value := s.quotaFitReport("recent", 6, false); value["computing"] != false || calls.Load() != 1 {
		t.Fatal("usage appends restarted the computing poll loop")
	}
	*clock = clock.Add(31 * time.Second)
	value := s.quotaFitReport("recent", 6, false)
	if value["refreshing"] != true {
		t.Fatal("source update ignored")
	}
	waitFitWorker(t, s)
	*clock = clock.Add(301 * time.Second)
	_ = s.quotaFitReport("recent", 6, false)
	waitFitWorker(t, s)
	if calls.Load() != 3 {
		t.Fatal("TTL/source refresh wrong")
	}
}

func TestQuotaFitControlEditsInvalidateWithoutThrottle(t *testing.T) {
	s, _ := fixtureServer(t)
	var calls atomic.Int32
	s.fitCache.compute = func(string, int) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"status": "stable", "computing": false}, nil
	}
	_ = s.quotaFitReport("recent", 6, false)
	waitFitWorker(t, s)
	if err := writeJSON(filepath.Join(s.options.Data, "gui.json"), map[string]any{"prices": map[string]any{"updated": true}}); err != nil {
		t.Fatal(err)
	}
	if value := s.quotaFitReport("recent", 6, false); value["refreshing"] != true {
		t.Fatal("pricing edit was hidden by the append throttle")
	}
	waitFitWorker(t, s)
	if calls.Load() != 2 {
		t.Fatal("pricing edit did not trigger a refit")
	}
}
func TestQuotaFitFailureIsExplicitAndBounded(t *testing.T) {
	s, _ := fixtureServer(t)
	var calls atomic.Int32
	s.fitCache.compute = func(string, int) (map[string]any, error) {
		calls.Add(1)
		return nil, errors.New("private underlying diagnostic")
	}
	_ = s.quotaFitReport("recent", 6, false)
	waitFitWorker(t, s)
	value := s.quotaFitReport("recent", 6, false)
	if value["status"] != "unavailable" || value["computing"] != false || calls.Load() != 1 || value["error"] == "private underlying diagnostic" {
		t.Fatal("failure hidden or retried without bound")
	}
}

func TestQuotaHistoryTailBounds(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		rows, err := tailQuotaHistory(filepath.Join(t.TempDir(), "absent"))
		if err != nil || len(rows) != 0 {
			t.Fatal("missing history should be empty")
		}
	})
	t.Run("malformed-and-final-line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "history")
		if err := os.WriteFile(path, []byte("invalid\n{\"ts\":1}\nnull\n{\"ts\":2}"), 0600); err != nil {
			t.Fatal(err)
		}
		rows, err := tailQuotaHistory(path)
		if err != nil || len(rows) != 2 || fitNumber(fitMap(rows[1])["ts"]) != 2 {
			t.Fatal("JSONL filtering changed")
		}
	})
	t.Run("tail-discards-large-partial-line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "history")
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 9*1024*1024)+"\n{\"ts\":3}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		rows, err := tailQuotaHistory(path)
		if err != nil || len(rows) != 1 || fitNumber(fitMap(rows[0])["ts"]) != 3 {
			t.Fatal("partial tail line should be discarded")
		}
	})
	t.Run("oversized-whole-line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "history")
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 2*1024*1024)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := tailQuotaHistory(path); err == nil {
			t.Fatal("whole oversized line accepted")
		}
	})
}
