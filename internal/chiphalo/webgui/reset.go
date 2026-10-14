package webgui

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

type resetCreditCache struct {
	mu        sync.Mutex
	key       string
	at        time.Time
	data      map[string]map[string]any
	computing bool
	done      chan struct{}
}

func resetUnavailable(status string) map[string]any {
	result := map[string]any{"status": status, "available_count": nil, "credits": []any{}}
	if status == "unavailable" {
		result["error"] = "official reset credits unavailable"
	}
	return result
}
func publicResetCredits(value any) map[string]any {
	source, ok := value.(map[string]any)
	if !ok || source["status"] != "ok" {
		return resetUnavailable("unavailable")
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return resetUnavailable("unavailable")
	}
	projected, err := quota.ProjectResetCredits(raw)
	if err != nil {
		return resetUnavailable("unavailable")
	}
	result := map[string]any{"status": "ok", "available_count": projected.Available, "credits": projected.Credits}
	if stamp, ok := source["updated_at"].(float64); ok && stamp >= 0 && stamp < 1e12 {
		result["updated_at"] = int64(stamp)
	}
	return result
}

func (s *Server) resetCreditResponse(w http.ResponseWriter, r *http.Request) {
	accounts, public, err := s.quotaAccounts()
	if err != nil {
		reject(w, 503, err.Error())
		return
	}
	respond(w, 200, map[string]any{"ok": true, "accounts": s.resetReports(accounts, public, r.URL.Query().Get("fresh") == "1")})
}

// resetReports is read-only and asynchronous. Slow official GETs do not hold the
// account-page request or return a different account's cached credits.
func (s *Server) resetReports(accounts []billing.Account, public []map[string]any, force bool) map[string]map[string]any {
	identity := []map[string]string{}
	for index, account := range accounts {
		identity = append(identity, map[string]string{"id": account.ID, "prefix": account.Prefix, "workspace": stringValue(public[index]["workspace_id"])})
	}
	raw, _ := json.Marshal(identity)
	key := string(raw)
	cache := &s.resetCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.data != nil && cache.key == key && s.options.Now().Sub(cache.at) < 30*time.Second && !force {
		return cache.data
	}
	if !cache.computing {
		cache.computing = true
		cache.done = make(chan struct{})
		go func() {
			value, err := s.call(context.Background(), "oauth_reset_credits", nil)
			var rows map[string]any
			if err == nil && value["ok"] == true {
				rows, _ = value["accounts"].(map[string]any)
			}
			data := map[string]map[string]any{}
			for _, account := range accounts {
				data[account.Prefix] = publicResetCredits(rows[account.Prefix])
			}
			cache.mu.Lock()
			defer cache.mu.Unlock()
			defer close(cache.done)
			cache.data, cache.key, cache.at = data, key, s.options.Now()
			cache.computing = false
		}()
	}
	if cache.data != nil && cache.key == key {
		return cache.data
	}
	data := map[string]map[string]any{}
	for _, account := range accounts {
		data[account.Prefix] = resetUnavailable("loading")
	}
	return data
}
