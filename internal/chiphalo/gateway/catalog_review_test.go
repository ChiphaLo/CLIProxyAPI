package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestCatalogReviewProducesAccountLocalCASWithoutProvisionalRegistration(t *testing.T) {
	live, f, _ := liveFixture(t)
	dir := filepath.Join(filepath.Dir(f.path), "config")
	files := map[string][]byte{"cliproxyapi.yaml": []byte("api-keys: [inner-test]\nforce-model-prefix: true\n"), "exposed-codex-models.json": []byte(`{"models":["gpt-6-astra","gpt-5.4"],"extension":"preserve"}`), "context-windows.json": []byte(`{"models":{"gpt-6-astra":{"context_window":272000,"supported_reasoning_levels":[{"effort":"medium"}]},"codex2/gpt-6-astra":{"context_window":272000,"supported_reasoning_levels":[{"effort":"medium"}]},"gpt-5.4":{"context_window":272000,"supported_reasoning_levels":[{"effort":"medium"}]},"provider/keep":{"extension":"keep"}}}`)}
	for name, raw := range files {
		atomicIdentityWrite(t, filepath.Join(dir, name), raw)
	}
	for prefix, a := range f.auths {
		auth := a.Clone()
		auth.Metadata["access_token"] = "catalog-token-" + prefix
		if _, err := f.manager.Update(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		hints, err := live.ModelHints(t.Context(), auth, stockCodexCatalog(auth))
		if err != nil {
			t.Fatal(err)
		}
		for _, hint := range hints {
			hint.ID = prefix + "/" + hint.ID
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", hints)
	}
	posts := map[string]int{}
	fetcher := quota.Fetcher{Transport: func(string) (http.RoundTripper, error) {
		return quotaRoundTrip(func(r *http.Request) (*http.Response, error) {
			prefix := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer catalog-token-")
			if prefix != "codex" && prefix != "codex2" {
				t.Fatal("catalog used a third credential")
			}
			body := `{"models":[{"slug":"gpt-6-astra"},{"slug":"gpt-new-` + prefix + `","context_window":272000,"supported_reasoning_levels":[{"effort":"medium"}],"input_modalities":["text"],"multi_agent_version":"v2"}]}`
			if r.Method == "POST" {
				posts[prefix]++
				var input struct {
					Model string `json:"model"`
				}
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &input)
				if input.Model != "gpt-new-"+prefix {
					t.Fatal("probe was not exact-account local")
				}
				body = `data: {"type":"response.completed","response":{"id":"resp_review","model":"` + input.Model + `","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":1,"total_tokens":8}}}` + "\n\n"
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}), nil
	}}
	publication, err := live.ReviewCatalog(t.Context(), filepath.Join(dir, "cliproxyapi.yaml"), f.manager, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(publication.NewModels, []string{"codex2/gpt-6-astra", "codex2/gpt-new-codex2", "gpt-6-astra", "gpt-new-codex"}) || posts["codex"] != 2 || posts["codex2"] != 2 {
		t.Fatal("review failed to preserve member catalog and fixed probes", publication.NewModels, posts)
	}
	for name, raw := range files {
		current, _ := os.ReadFile(filepath.Join(dir, name))
		if !reflect.DeepEqual(current, raw) {
			t.Fatal("review published a file before transaction")
		}
	}
	for _, a := range f.auths {
		if registry.GetGlobalRegistry().ClientSupportsModel(a.ID, a.Prefix+"/gpt-new-"+a.Prefix) {
			t.Fatal("review provisionally registered new model")
		}
	}
	atomicIdentityWrite(t, filepath.Join(dir, "exposed-codex-models.json"), publication.Exposed)
	atomicIdentityWrite(t, filepath.Join(dir, "context-windows.json"), publication.Context)
	for _, auth := range f.auths {
		hints, err := live.ModelHints(t.Context(), auth, stockCodexCatalog(auth))
		if err != nil {
			t.Fatal(err)
		}
		for _, hint := range hints {
			hint.ID = auth.Prefix + "/" + hint.ID
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", hints)
	}
	if result := live.ReviewedReadiness(t.Context(), &config.Config{}, f.manager); !result.Ready {
		t.Fatal("complete reviewed metadata was not actually ready", result)
	}
}
