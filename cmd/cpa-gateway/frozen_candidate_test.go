package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/clientkeys"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

// This opt-in Linux integration test uses only a private frozen snapshot and a
// network-none container. It sends no inference request and consumes no quota.
func TestFrozenCandidateCatalogKeysProvidersAndHistoricalRetention(t *testing.T) {
	root, binary := os.Getenv("CPA_FROZEN_ROOT"), os.Getenv("CPA_SHADOW_BINARY")
	if root == "" || binary == "" || runtime.GOOS != "linux" {
		t.Skip("explicit Linux private snapshot required")
	}
	base := filepath.Join(root, "opt/unified-ai-gateway")
	configPath := filepath.Join(base, "config/cliproxyapi.yaml")
	keyPath := filepath.Join(base, "data/api-keys.json")
	envPath := filepath.Join(base, "gateway.env")
	modelsPath := filepath.Join(base, "config/exposed-codex-models.json")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("private source YAML missing")
	}
	cfg, err := config.ParseConfigBytes(raw)
	if err != nil {
		t.Fatal("private source YAML not compatible")
	}
	admin, err := clientkeys.ReadLegacyAdminKey(envPath)
	if err != nil {
		t.Fatal("private admin environment not compatible")
	}
	keys, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	models, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Keep a failed validation retry usable, without ever touching live data.
		for path, data := range map[string][]byte{configPath: raw, keyPath: keys, modelsPath: models} {
			if err := os.WriteFile(path+".shadow-restore", data, 0600); err == nil {
				_ = os.Rename(path+".shadow-restore", path)
			}
		}
	}()
	protected := []string{configPath, envPath, modelsPath, filepath.Join(base, "config/oauth-accounts.json"), filepath.Join(base, "data/usage.jsonl"), filepath.Join(base, "data/oauth-quota-history.jsonl")}
	entries, _ := os.ReadDir(filepath.Join(base, "auth"))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			protected = append(protected, filepath.Join(base, "auth", entry.Name()))
		}
	}
	before := map[string][32]byte{}
	for _, path := range protected {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("required private history missing")
		}
		before[path] = sha256.Sum256(data)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	logs, err := os.CreateTemp("", "cpa-shadow-runtime-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logs.Close() }()
	cmd := exec.Command(binary, "--legacy-candidate", "--candidate-root", root, "--candidate-listen", address, "--config", configPath,
		"--legacy-config-dir", filepath.Join(base, "config"), "--legacy-auth-dir", filepath.Join(base, "auth"), "--legacy-key-store", keyPath, "--legacy-env", envPath,
		"--receipts", filepath.Join(base, "data/usage.jsonl"), "--audit", filepath.Join(base, "data/audit.jsonl"))
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		if err := cmd.Wait(); err != nil {
			t.Error("shadow runtime did not drain cleanly")
		}
	}()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	request := func(token, path string) (int, []byte) {
		req, _ := http.NewRequest("GET", "http://"+address+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(req)
		if err != nil {
			return 0, nil
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
		if err != nil {
			return 0, nil
		}
		return response.StatusCode, body
	}
	waitFor := func(check func() bool) {
		t.Helper()
		deadline := time.NewTimer(12 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(400 * time.Millisecond)
		defer ticker.Stop()
		for {
			if check() {
				return
			}
			select {
			case <-deadline.C:
				t.Fatal("shadow condition did not become ready; inspect private runtime log")
			case <-ticker.C:
			}
		}
	}
	waitFor(func() bool { status, _ := request("", "/health"); return status == 200 })
	waitFor(func() bool {
		status, body := request("", "/__cpa_release/ready")
		return status == 200 && bytes.Contains(body, []byte(`"ready":true`))
	})
	if status, body := request("", "/__cpa_release/ready"); status != 200 || !bytes.Contains(body, []byte(`"production_ready":false`)) || !bytes.Contains(body, []byte(`"credential_refresh":false`)) {
		t.Fatal("shadow advertised production activation or refresh ownership")
	}
	if status, _ := request("", "/v1/models"); status != 401 {
		t.Fatal("anonymous catalog access accepted")
	}
	catalog := func(token string) map[string]bool {
		status, body := request(token, "/v1/models")
		if status != 200 {
			return nil
		}
		var doc struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &doc) != nil {
			return nil
		}
		ids := map[string]bool{}
		for _, row := range doc.Data {
			ids[row.ID] = true
		}
		return ids
	}
	waitFor(func() bool { return len(catalog(admin)) > 0 })
	ids := catalog(admin)
	for _, provider := range cfg.OpenAICompatibility {
		for _, model := range provider.Models {
			alias := model.Alias
			if alias == "" {
				alias = model.Name
			}
			name := alias
			if provider.Prefix != "" {
				name = provider.Prefix + "/" + alias
			}
			if !ids[name] {
				t.Errorf("configured provider model absent: %s", name)
			}
		}
	}
	if len(cfg.OpenAICompatibility) == 0 {
		t.Fatal("snapshot contains no current API provider")
	}
	if !ids["gpt-6-astra"] || !ids["codex2/gpt-6-astra"] || ids["codex/gpt-6-astra"] {
		t.Fatal("OAuth account catalogs lost or primary prefix duplicated")
	}
	for _, name := range []string{"gpt-6-luna", "gpt-6.1-sol", "codex2/gpt-6-luna", "codex2/gpt-6.1-sol"} {
		if !ids[name] {
			t.Fatalf("current production OAuth model missing: %s", name)
		}
	}
	if len(ids) != 13 {
		t.Fatal("candidate catalog differs from the current 13-model production surface")
	}
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path+".shadow-edit", data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".shadow-edit", path); err != nil {
			t.Fatal(err)
		}
	}
	// Some deployments explicitly issue the same bytes as an internal key.
	// That existing public key must remain usable. Disable its issued identity
	// in this private copy to prove config-inline cannot resurrect the secret.
	for _, internal := range cfg.APIKeys {
		var doc struct {
			Keys []map[string]any `json:"keys"`
		}
		if err := json.Unmarshal(keys, &doc); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(internal))
		matched := false
		for _, entry := range doc.Keys {
			if entry["hash"] == hex.EncodeToString(digest[:]) {
				entry["enabled"] = false
				matched = true
			}
		}
		if internal == admin {
			continue
		}
		if matched {
			disabled, _ := json.Marshal(doc)
			write(keyPath, disabled)
		}
		if status, _ := request(internal, "/v1/models"); status != 401 {
			t.Fatalf("disabled/unissued backend request returned %d, expected 401", status)
		}
		if matched {
			write(keyPath, keys)
		}
	}
	var keyDoc map[string]any
	if err := json.Unmarshal(keys, &keyDoc); err != nil {
		t.Fatal(err)
	}
	keyEntries, ok := keyDoc["keys"].([]any)
	if !ok {
		t.Fatal("issued key schema incompatible")
	}
	token := "shadow-issued-key-fixture-only"
	digest := sha256.Sum256([]byte(token))
	entry := map[string]any{"id": "shadow-validation-key", "hash": hex.EncodeToString(digest[:]), "enabled": true}
	keyDoc["keys"] = append(keyEntries, entry)
	added, _ := json.Marshal(keyDoc)
	write(keyPath, added)
	if catalog(token) == nil {
		t.Fatal("GUI-style key issue was not applied")
	}
	entry["enabled"] = false
	added, _ = json.Marshal(keyDoc)
	write(keyPath, added)
	if status, _ := request(token, "/v1/models"); status != 401 {
		t.Fatal("GUI-style key disable was not applied")
	}
	write(keyPath, keys)
	if len(catalog(admin)) == 0 {
		t.Fatal("restoring original key list lost current providers")
	}
	write(modelsPath, []byte(`{"models":["gpt-6-luna"]}`))
	filtered := catalog(admin)
	if filtered["gpt-6-astra"] || filtered["codex2/gpt-6-astra"] || !filtered["gpt-6-luna"] {
		t.Fatal("GUI OAuth model filter was not applied to both fixed account catalogs")
	}
	write(modelsPath, models)
	// Edit only the provider sequence, exactly as a GUI legacy YAML publication
	// can do. Do not upgrade/rewrite the source schema during a language port.
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	var sequence *yaml.Node
	var find func(*yaml.Node)
	find = func(node *yaml.Node) {
		if node.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(node.Content); i += 2 {
				if node.Content[i].Value == "openai-compatibility" && node.Content[i+1].Kind == yaml.SequenceNode {
					sequence = node.Content[i+1]
				} else {
					find(node.Content[i+1])
				}
			}
		} else {
			for _, child := range node.Content {
				find(child)
			}
		}
	}
	find(&document)
	if sequence == nil || len(sequence.Content) != len(cfg.OpenAICompatibility) {
		t.Fatal("cannot validate actual legacy GUI provider schema")
	}
	removed := cfg.OpenAICompatibility[0].Prefix
	if removed == "" {
		t.Fatal("snapshot provider lacks fixed prefix")
	}
	sequence.Content = sequence.Content[1:]
	edited, err := yaml.Marshal(&document)
	if err != nil {
		t.Fatal(err)
	}
	write(configPath, edited)
	waitFor(func() bool {
		set := catalog(admin)
		if len(set) == 0 {
			return false
		}
		for name := range set {
			if strings.HasPrefix(name, removed+"/") {
				return false
			}
		}
		return true
	})
	write(configPath, raw)
	waitFor(func() bool {
		set := catalog(admin)
		for name := range ids {
			if !set[name] {
				return false
			}
		}
		return true
	})
	for path, expected := range before {
		data, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(data) != expected {
			t.Errorf("shadow changed protected source/history or credential: %s", filepath.Base(path))
		}
	}
	afterKeys, _ := os.ReadFile(keyPath)
	if !bytes.Equal(keys, afterKeys) {
		t.Fatal("original issued-key data not restored in private snapshot")
	}
	t.Logf("private snapshot verified: %d providers, %d public models, %d protected files; no inference or refresh", len(cfg.OpenAICompatibility), len(ids), len(before))
}
