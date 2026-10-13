package webgui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func clone(value map[string]any) map[string]any {
	raw, _ := json.Marshal(value)
	var result map[string]any
	_ = json.Unmarshal(raw, &result)
	return result
}
func stringValue(value any) string { v, _ := value.(string); return strings.TrimSpace(v) }
func number(value any) float64     { v, _ := value.(float64); return v }
func (s *Server) state() (map[string]any, error) {
	return s.loadRecoverableState()
}
func (s *Server) saveState(state map[string]any) error {
	path := filepath.Join(s.options.Data, "state.json")
	raw, err := privatefile.Read(path, 4*1024*1024, false)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.options.Data, "backups")
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("state backup directory unsafe")
	}
	file, err := os.CreateTemp(dir, "state-go-*.json")
	if err != nil {
		return err
	}
	name := file.Name()
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	if e := file.Close(); err == nil {
		err = e
	}
	if err != nil || syncParent(dir) != nil {
		return errors.New("state backup durability unconfirmed")
	}
	state["updatedAt"] = s.options.Now().Unix()
	if err := s.commitState(state, raw); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	owned := []string{}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "state-go-") && strings.HasSuffix(entry.Name(), ".json") {
			owned = append(owned, entry.Name())
		}
	}
	// Only this implementation's backup names are eligible for pruning. Legacy
	// recovery snapshots and tombstones are never removed during migration.
	slices.SortFunc(owned, func(a, b string) int {
		ia, _ := os.Stat(filepath.Join(dir, a))
		ib, _ := os.Stat(filepath.Join(dir, b))
		if ia == nil || ib == nil {
			return strings.Compare(a, b)
		}
		return ia.ModTime().Compare(ib.ModTime())
	})
	if len(owned) > 20 {
		for _, entry := range owned[:len(owned)-20] {
			if filepath.Join(dir, entry) != name {
				_ = os.Remove(filepath.Join(dir, entry))
			}
		}
	}
	return nil
}
func (s *Server) publicProviders(ctx context.Context, state map[string]any) ([]any, error) {
	status, err := s.call(ctx, "status", nil)
	if err != nil || status["ok"] != true {
		return nil, errors.New("provider credential status unavailable")
	}
	secrets, _ := status["provider_secrets"].(map[string]any)
	out := []any{}
	for _, value := range state["providers"].([]any) {
		p, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("invalid provider inventory")
		}
		safe := clone(p)
		for _, key := range []string{"apiKey", "api_key", "secret", "headers"} {
			delete(safe, key)
		}
		safe["hasApiKey"] = secrets[stringValue(p["id"])] == true
		out = append(out, safe)
	}
	return out, nil
}
func (s *Server) stateResponse(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.state()
	if err != nil {
		reject(w, 503, err.Error())
		return
	}
	providers, err := s.publicProviders(r.Context(), state)
	if err != nil {
		reject(w, 503, err.Error())
		return
	}
	keys, err := s.call(r.Context(), "read_api_keys", nil)
	if err != nil || keys["ok"] != true {
		reject(w, 503, "keys unavailable")
		return
	}
	seed, err := s.call(r.Context(), "gui_provider_seed", nil)
	if err != nil || seed["ok"] != true {
		reject(w, 503, "model context unavailable")
		return
	}
	var exposed struct {
		Models []string `json:"models"`
	}
	if readJSON(s.options.Exposed, 1024*1024, false, &exposed) != nil {
		reject(w, 503, "OAuth model inventory unavailable")
		return
	}
	respond(w, 200, map[string]any{"providers": providers, "keys": keys["keys"], "codex": map[string]any{"models": exposed.Models, "context": seed["codex_context"]}, "stateRecovery": state["recovery"]})
}
func (s *Server) statusResponse(w http.ResponseWriter, r *http.Request) {
	value, err := s.call(r.Context(), "status", nil)
	if err != nil || value["ok"] != true {
		reject(w, 503, "status unavailable")
		return
	}
	keys, err := s.call(r.Context(), "read_api_keys", nil)
	if err != nil {
		reject(w, 503, "keys unavailable")
		return
	}
	s.mu.Lock()
	state, err := s.state()
	s.mu.Unlock()
	if err != nil {
		reject(w, 503, err.Error())
		return
	}
	var exposed struct {
		Models []string `json:"models"`
	}
	if readJSON(s.options.Exposed, 1024*1024, false, &exposed) != nil {
		reject(w, 503, "models unavailable")
		return
	}
	rows, _ := keys["keys"].([]any)
	result := map[string]any{"status": value["status"], "keys": len(rows), "codexModels": len(exposed.Models), "providers": len(state["providers"].([]any))}
	if r.URL.Query().Get("include_usage") != "0" {
		summary, err := s.usage(r.URL.Query())
		if err != nil {
			reject(w, 503, "usage unavailable")
			return
		}
		result["usage"] = summary["totals"]
	}
	respond(w, 200, result)
}
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	state, err := s.state()
	s.mu.Unlock()
	if err != nil {
		reject(w, 503, err.Error())
		return
	}
	s.forward(w, r, "apply_bundle", map[string]any{"state": map[string]any{"providers": state["providers"], "managedPrefixes": state["managedPrefixes"]}})
}
func (s *Server) normalizeProvider(ctx context.Context, p map[string]any, old map[string]any) error {
	id, prefix := stringValue(p["id"]), stringValue(p["prefix"])
	if !identifier.MatchString(id) || !identifier.MatchString(prefix) || prefix == "codex" || prefix == "codex2" {
		return errors.New("invalid or reserved provider identity")
	}
	base, proxy := strings.TrimRight(stringValue(p["baseUrl"]), "/"), stringValue(p["proxyUrl"])
	if old == nil || base != stringValue(old["baseUrl"]) || proxy != stringValue(old["proxyUrl"]) {
		if err := configbundle.ValidateProviderOriginWithResolver(ctx, base, proxy, s.options.Resolve); err != nil {
			return errors.New("provider URL or proxy rejected")
		}
	}
	p["baseUrl"], p["proxyUrl"] = base, proxy
	if p["name"] == nil {
		p["name"] = id
	}
	name, validName := p["name"].(string)
	if !validName || len(name) > 120 {
		return errors.New("invalid provider display name")
	}
	if p["enabled"] == nil {
		p["enabled"] = true
	}
	if _, ok := p["enabled"].(bool); !ok {
		return errors.New("enabled must be boolean")
	}
	models, ok := p["models"].([]any)
	if !ok {
		return errors.New("models must be an array")
	}
	seen := map[string]bool{}
	for _, raw := range models {
		m, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid model")
		}
		raw, _ := json.Marshal(m)
		normalized, err := configbundle.NormalizeModelDraft(raw)
		if err != nil {
			return errors.New("invalid model configuration")
		}
		var fields map[string]any
		if json.Unmarshal(normalized, &fields) != nil {
			return errors.New("invalid model configuration")
		}
		for key, value := range fields {
			m[key] = value
		}
		client := stringValue(m["clientName"])
		if seen[client] {
			return errors.New("duplicate local model name")
		}
		seen[client] = true
	}
	delete(p, "apiKey")
	delete(p, "clearApiKey")
	p["updatedAt"] = s.options.Now().Unix()
	return nil
}
func (s *Server) providers(w http.ResponseWriter, r *http.Request, path string, body map[string]any) {
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	state, err := s.state()
	if err != nil {
		reject(w, 503, err.Error())
		return
	}
	rows := state["providers"].([]any)
	if path == "/api/providers" && r.Method == "POST" {
		if len(rows) >= 128 {
			reject(w, 409, "provider inventory limit reached")
			return
		}
		p := clone(body)
		id := stringValue(p["id"])
		if id == "" {
			id = slugify(stringValue(p["name"]))
		}
		baseID := id
		for n := 2; containsProvider(rows, id); n++ {
			id = fmt.Sprintf("%s-%d", baseID, n)
		}
		p["id"] = id
		if p["prefix"] == nil || p["prefix"] == "" {
			p["prefix"] = id
		}
		if p["models"] == nil {
			p["models"] = []any{}
		}
		p["createdAt"] = s.options.Now().Unix()
		if err := s.normalizeProvider(r.Context(), p, nil); err != nil {
			reject(w, 400, err.Error())
			return
		}
		for _, row := range rows {
			v, _ := row.(map[string]any)
			if v["prefix"] == p["prefix"] {
				reject(w, 409, "provider prefix already exists")
				return
			}
		}
		if key := stringValue(body["apiKey"]); key != "" {
			value, err := s.call(r.Context(), "set_provider_secret", map[string]any{"provider_id": id, "secret": key})
			if err != nil || value["ok"] != true {
				reject(w, 503, "provider secret update failed")
				return
			}
		}
		state["providers"] = append(rows, p)
		clearTombstone(state, id)
		appendPrefix(state, stringValue(p["prefix"]))
		if s.saveState(state) != nil {
			reject(w, 503, "provider draft outcome must be checked; apply was not started")
			return
		}
		p["hasApiKey"] = stringValue(body["apiKey"]) != ""
		respond(w, 200, map[string]any{"ok": true, "provider": p})
		return
	}
	tail := strings.TrimPrefix(path, "/api/providers/")
	parts := strings.Split(tail, "/")
	id := parts[0]
	if len(parts) > 2 || !identifier.MatchString(id) {
		reject(w, 404, "provider not found")
		return
	}
	index := -1
	var original map[string]any
	for i, row := range rows {
		p, _ := row.(map[string]any)
		if p["id"] == id {
			index = i
			original = p
			break
		}
	}
	if index < 0 {
		reject(w, 404, "provider not found")
		return
	}
	if len(parts) == 2 && (parts[1] == "models" || parts[1] == "test") && r.Method == "POST" {
		base := stringValue(original["baseUrl"])
		proxy := stringValue(original["proxyUrl"])
		if body["baseUrl"] != nil {
			base = stringValue(body["baseUrl"])
		}
		if body["proxyUrl"] != nil {
			proxy = stringValue(body["proxyUrl"])
		}
		policy := "direct"
		if proxy != "" {
			policy = "cpa-egress"
		}
		payload := map[string]any{"provider_id": id, "base_url": base, "proxy_policy": policy, "proxy_url": proxy}
		if key := stringValue(body["apiKey"]); key != "" {
			payload["api_key"] = key
		}
		// The immutable probe snapshot must not block unrelated GUI state reads
		// while a public provider is slow or unreachable.
		locked = false
		s.mu.Unlock()
		s.providerProbe(w, r, payload)
		return
	}
	if len(parts) != 1 {
		reject(w, 404, "not found")
		return
	}
	if r.Method == "PUT" {
		p := clone(original)
		for _, key := range []string{"name", "prefix", "baseUrl", "proxyUrl", "enabled", "models"} {
			if value, ok := body[key]; ok {
				p[key] = value
			}
		}
		if err := s.normalizeProvider(r.Context(), p, original); err != nil {
			reject(w, 400, err.Error())
			return
		}
		for i, row := range rows {
			v, _ := row.(map[string]any)
			if i != index && v["prefix"] == p["prefix"] {
				reject(w, 409, "provider prefix already exists")
				return
			}
		}
		op := ""
		payload := map[string]any{"provider_id": id}
		if key := stringValue(body["apiKey"]); key != "" {
			op = "set_provider_secret"
			payload["secret"] = key
		} else if body["clearApiKey"] == true {
			op = "delete_provider_secret"
		}
		if op != "" {
			value, err := s.call(r.Context(), op, payload)
			if err != nil || value["ok"] != true {
				reject(w, 503, "provider secret update failed")
				return
			}
		}
		rows[index] = p
		clearTombstone(state, id)
		appendPrefix(state, stringValue(original["prefix"]))
		appendPrefix(state, stringValue(p["prefix"]))
		if s.saveState(state) != nil {
			reject(w, 503, "provider draft outcome must be checked; apply was not started")
			return
		}
		respond(w, 200, map[string]any{"ok": true, "provider": p})
		return
	}
	if r.Method == "DELETE" {
		state["providers"] = append(rows[:index:index], rows[index+1:]...)
		appendPrefix(state, stringValue(original["prefix"]))
		tombstones, _ := state["removedProviders"].([]any)
		state["removedProviders"] = append(tombstones, map[string]any{"id": id, "prefix": original["prefix"], "removedAt": s.options.Now().Unix()})
		// Retain the secret until draft removal is durable; published configuration
		// is untouched until a later explicitly requested apply transaction.
		if s.saveState(state) != nil {
			reject(w, 503, "provider draft removal failed")
			return
		}
		s.forward(w, r, "delete_provider_secret", map[string]any{"provider_id": id})
		return
	}
	reject(w, 405, "method not allowed")
}

func clearTombstone(state map[string]any, id string) {
	rows, _ := state["removedProviders"].([]any)
	out := []any{}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if ok && row["id"] == id {
			continue
		}
		out = append(out, raw)
	}
	state["removedProviders"] = out
}
func slugify(value string) string {
	value = strings.ToLower(value)
	out := []byte{}
	for _, c := range []byte(value) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			out = append(out, c)
		} else {
			out = append(out, '-')
		}
	}
	value = strings.Trim(string(out), "-")
	if len(value) > 48 {
		value = value[:48]
	}
	if value == "" {
		value = "provider"
	}
	return value
}
func containsProvider(rows []any, id string) bool {
	for _, row := range rows {
		p, _ := row.(map[string]any)
		if p["id"] == id {
			return true
		}
	}
	return false
}
func appendPrefix(state map[string]any, prefix string) {
	if prefix == "" {
		return
	}
	values, _ := state["managedPrefixes"].([]any)
	for _, value := range values {
		if value == prefix {
			return
		}
	}
	state["managedPrefixes"] = append(values, prefix)
}
func (s *Server) prices() (map[string]*billing.Rates, error) {
	var doc struct {
		Prices map[string]*billing.Rates `json:"prices"`
	}
	if readJSON(filepath.Join(s.options.Data, "gui.json"), 2*1024*1024, true, &doc) != nil {
		return nil, errors.New("pricing unavailable")
	}
	if doc.Prices == nil {
		doc.Prices = map[string]*billing.Rates{}
	}
	for _, rate := range doc.Prices {
		if rate == nil {
			return nil, errors.New("invalid prices")
		}
		for _, value := range []float64{rate.Input, rate.Cached, rate.Write, rate.Output} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1000000 {
				return nil, errors.New("invalid price")
			}
		}
	}
	return doc.Prices, nil
}
func (s *Server) pricesRoute(w http.ResponseWriter, r *http.Request, body map[string]any) {
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	if r.Method == "GET" {
		prices, err := s.prices()
		if err != nil {
			reject(w, 503, err.Error())
			return
		}
		respond(w, 200, map[string]any{"prices": prices})
		return
	}
	if r.Method != "PUT" {
		reject(w, 405, "method not allowed")
		return
	}
	var prices map[string]*billing.Rates
	raw, _ := json.Marshal(body["prices"])
	if privatefile.Decode(raw, &prices) != nil || prices == nil || len(prices) > 10000 {
		reject(w, 400, "invalid prices")
		return
	}
	for model, rate := range prices {
		if model == "" || len(model) > 256 || rate == nil {
			reject(w, 400, "invalid prices")
			return
		}
		for _, v := range []float64{rate.Input, rate.Cached, rate.Write, rate.Output} {
			if v < 0 || v > 1000000 || math.IsNaN(v) || math.IsInf(v, 0) {
				reject(w, 400, "invalid price")
				return
			}
		}
	}
	var doc map[string]json.RawMessage
	if readJSON(s.auth.config, 2*1024*1024, true, &doc) != nil {
		reject(w, 503, "GUI data unavailable")
		return
	}
	doc["prices"], _ = json.Marshal(prices)
	if writeJSON(s.auth.config, doc) != nil {
		reject(w, 503, "price durability unconfirmed")
		return
	}
	respond(w, 200, map[string]any{"ok": true, "prices": prices})
}
