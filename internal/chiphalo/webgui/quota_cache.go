package webgui

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quotafit"
)

type quotaFitKey struct {
	Window    string
	Cycles    int
	Signature string
	Control   string
}
type quotaFitCache struct {
	mu        sync.Mutex
	key       quotaFitKey
	at        time.Time
	data      map[string]any
	computing bool
	inflight  quotaFitKey
	err       bool
	errAt     time.Time
	done      chan struct{}
	compute   func(string, int) (map[string]any, error)
}

func (s *Server) fitSignature() string {
	history := s.options.History
	if history == "" {
		history = filepath.Join(filepath.Dir(s.options.Quota), "oauth-quota-history.jsonl")
	}
	paths := []string{history, s.options.Usage, s.options.Quota, s.options.Exposed, filepath.Join(s.options.Data, "quota-model.json"), filepath.Join(s.options.Data, "gui.json"), filepath.Join(s.options.Data, "oauth-billing-resets.json")}
	return fitFileSignature(paths)
}
func (s *Server) fitControlSignature() string {
	return fitFileSignature([]string{s.options.Quota, s.options.Exposed, filepath.Join(s.options.Data, "quota-model.json"), filepath.Join(s.options.Data, "gui.json"), filepath.Join(s.options.Data, "oauth-billing-resets.json")})
}
func fitFileSignature(paths []string) string {
	parts := []string{}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			parts = append(parts, path+":missing")
			continue
		}
		parts = append(parts, path+":"+strconv.FormatInt(info.Size(), 10)+":"+strconv.FormatInt(info.ModTime().UnixNano(), 10)+":"+strconv.Itoa(int(info.Mode()))+":"+quotaFileIdentity(info))
	}
	return strings.Join(parts, "|")
}
func (s *Server) quotaFitReport(window string, cycles int, force bool) map[string]any {
	key := quotaFitKey{Window: window, Cycles: cycles, Signature: s.fitSignature(), Control: s.fitControlSignature()}
	cache := &s.fitCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := s.options.Now()
	if cache.data != nil && cache.key == key && now.Sub(cache.at) < 300*time.Second && !force {
		return cache.data
	}
	// The empirical fit is explicitly as-of, not a per-request live counter.
	// Appends must not turn the UI's 2.5-second computing poll into a permanent
	// bootstrap/refit loop. Account, pricing and exclusion edits still invalidate.
	if cache.data != nil && cache.key.Window == window && cache.key.Cycles == cycles && cache.key.Control == key.Control && now.Sub(cache.at) < 30*time.Second && !force {
		return cache.data
	}
	if cache.err && cache.inflight == key && now.Sub(cache.errAt) < 10*time.Second && !force {
		return map[string]any{"version": quotafit.Version, "status": "unavailable", "computing": false, "generated_at": nil, "accounts": []any{}, "pooled": nil, "error": "quota model computation unavailable"}
	}
	if !cache.computing {
		cache.computing = true
		cache.inflight = key
		cache.done = make(chan struct{})
		go func() {
			data, err := cache.compute(window, cycles)
			cache.mu.Lock()
			defer cache.mu.Unlock()
			defer close(cache.done)
			cache.computing = false
			cache.err = err != nil
			cache.errAt = s.options.Now()
			// Publish a completed as-of report even if usage appended during
			// fitting. The next poll refreshes it; traffic must not starve output.
			if err == nil {
				cache.key = key
				cache.data = data
				cache.at = s.options.Now()
			}
		}()
	}
	if cache.data != nil && cache.key.Window == window && cache.key.Cycles == cycles {
		data := shallowCopy(cache.data)
		data["computing"] = true
		data["refreshing"] = true
		return data
	}
	if cache.err && !cache.computing {
		return map[string]any{"version": quotafit.Version, "status": "unavailable", "computing": false, "generated_at": nil, "accounts": []any{}, "pooled": nil, "error": "quota model computation unavailable"}
	}
	return map[string]any{"version": quotafit.Version, "status": "computing", "computing": true, "generated_at": nil, "accounts": []any{}, "pooled": nil, "message": "quota model fit is being computed"}
}
