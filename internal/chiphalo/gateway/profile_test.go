package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const identityModel = "gpt-identity-fixture"

type profileExecutor struct {
	sink     *ReceiptSink
	selected []string
	failure  map[string]int
	results  int
}

func (*profileExecutor) Identifier() string { return "codex" }
func (e *profileExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req exec.Request, _ exec.Options) (exec.Response, error) {
	e.selected = append(e.selected, auth.ID)
	if code := e.failure[auth.ID]; code != 0 {
		return exec.Response{}, &coreauth.Error{HTTPStatus: code, Message: "fixture upstream failure"}
	}
	meta, ok := ctx.Value(metadataKey{}).(requestMetadata)
	if !ok || !usage.RawUsageCaptureEnabled(ctx) {
		return exec.Response{}, errors.New("admission context was lost")
	}
	e.sink.HandleUsage(ctx, usage.Record{AuthID: auth.ID, Model: req.Model, ResponseModel: identityModel,
		RequestedAt: meta.Started, Latency: time.Millisecond, RawUsageFormat: "responses", RawUsage: []byte(`{"input_tokens":10,"output_tokens":5,"total_tokens":15}`)})
	return exec.Response{Payload: []byte(`{"id":"resp_fixture","object":"response","status":"completed","model":"gpt-identity-fixture","output":[]}`)}, nil
}
func (e *profileExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req exec.Request, opts exec.Options) (*exec.StreamResult, error) {
	_, err := e.Execute(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan exec.StreamChunk, 2)
	chunks <- exec.StreamChunk{Payload: []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fixture\",\"model\":\"gpt-identity-fixture\",\"status\":\"in_progress\",\"output\":[]}}\n\n")}
	chunks <- exec.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"model\":\"gpt-identity-fixture\",\"status\":\"completed\",\"output\":[]}}\n\n")}
	close(chunks)
	return &exec.StreamResult{Chunks: chunks}, nil
}
func (e *profileExecutor) CountTokens(ctx context.Context, a *coreauth.Auth, r exec.Request, o exec.Options) (exec.Response, error) {
	return e.Execute(ctx, a, r, o)
}
func (*profileExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}
func (*profileExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected HTTP")
}

type innerKeyProvider struct{}

func (innerKeyProvider) Identifier() string { return "identity-inner-key-fixture" }
func (innerKeyProvider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if r.Header.Get("Authorization") == "Bearer inner-test-key" {
		return &sdkaccess.Result{Principal: "inner-test-key"}, nil
	}
	return nil, sdkaccess.NewInvalidCredentialError()
}

type profileFixture struct {
	engine   *gin.Engine
	profile  *LegacyProfile
	manager  *coreauth.Manager
	executor *profileExecutor
	path     string
	auths    map[string]*coreauth.Auth
}

func newProfileFixture(t *testing.T, disabled bool) *profileFixture {
	t.Helper()
	dir := t.TempDir()
	configDir, authDir := filepath.Join(dir, "config"), filepath.Join(dir, "auth")
	for _, p := range []string{configDir, authDir} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, v any) {
		raw, err := json.Marshal(v)
		if err == nil {
			err = os.WriteFile(path, raw, 0600)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	rows := []map[string]any{}
	auths := map[string]*coreauth.Auth{}
	for _, prefix := range []string{"codex", "codex2"} {
		id := prefix + "-fixture.json"
		member := prefix + "-member"
		claims, _ := json.Marshal(map[string]string{"sub": member, "email": member + "@example.invalid"})
		meta := map[string]any{"type": "codex", "prefix": prefix, "account_id": "same-team", "email": member + "@example.invalid", "id_token": "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".test", "disabled": disabled && prefix == "codex"}
		write(filepath.Join(authDir, id), meta)
		rows = append(rows, map[string]any{"id": member, "prefix": prefix, "auth_file": id, "workspace_id": "same-team"})
		auths[prefix] = &coreauth.Auth{ID: id, Provider: "codex", Prefix: prefix, Status: coreauth.StatusActive, Metadata: meta}
	}
	write(filepath.Join(configDir, "oauth-accounts.json"), map[string]any{"accounts": rows})
	write(filepath.Join(configDir, "exposed-codex-models.json"), map[string]any{"models": []string{identityModel}})
	digest := sha256.Sum256([]byte("issued-test-key"))
	keyPath := filepath.Join(dir, "keys.json")
	write(keyPath, map[string]any{"keys": []map[string]any{{"id": "issued-fixture", "hash": hex.EncodeToString(digest[:]), "enabled": true}}})
	p, err := LoadLegacyProfile("admin-test-key", keyPath, configDir, authDir)
	if err != nil {
		t.Fatal(err)
	}
	sinkPath := filepath.Join(dir, "usage.jsonl")
	sink, err := OpenReceiptSink(sinkPath, p.ReceiptBindings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	e := &profileExecutor{sink: sink, failure: map[string]int{}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(e)
	manager.SetRetryConfig(0, 0, 2)
	manager.SetResultPolicy(coreauth.ResultPolicyFunc(func(_ context.Context, result coreauth.Result) coreauth.Result { e.results++; return result }))
	for _, a := range auths {
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: a.Prefix + "/" + identityModel}, {ID: identityModel}, {ID: "gpt-unpublished-fixture"}})
		id := a.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	sdkaccess.RegisterProvider("identity-inner-key-fixture", innerKeyProvider{})
	p.RegisterAccess()
	t.Cleanup(func() {
		sdkaccess.UnregisterProvider("identity-inner-key-fixture")
		sdkaccess.UnregisterProvider(p.Keys.Identifier())
		sdkaccess.ClearExclusiveProvider()
	})
	access := sdkaccess.NewManager()
	access.SetProviders(sdkaccess.RegisteredProviders())
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	responses := openai.NewOpenAIResponsesAPIHandler(base)
	engine := gin.New()
	_ = engine.SetTrustedProxies(nil)
	engine.Use(api.AuthMiddleware(access), p.Admission(), p.ModelCatalog(base))
	engine.POST("/v1/responses", responses.Responses)
	engine.GET("/v1/models", func(c *gin.Context) { t.Fatal("CPA catalog did not handle model listing") })
	return &profileFixture{engine: engine, profile: p, manager: manager, executor: e, path: sinkPath, auths: auths}
}

func TestLegacyProfilePublishesDistinctAccountCatalogs(t *testing.T) {
	for _, query := range []string{"", "?client_version=0.162.1"} {
		t.Run(query, func(t *testing.T) {
			f := newProfileFixture(t, false)
			request := func(key string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodGet, "/v1/models"+query, nil)
				r.Header.Set("Authorization", "Bearer "+key)
				w := httptest.NewRecorder()
				f.engine.ServeHTTP(w, r)
				return w
			}
			if w := request("inner-test-key"); w.Code < 400 {
				t.Fatal("internal backend key bypassed catalog authentication")
			}
			w := request("admin-test-key")
			if w.Code != 200 {
				t.Fatalf("catalog failed %d %s", w.Code, w.Body.String())
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			var models []map[string]any
			field := "data"
			idField := "id"
			if query != "" {
				field = "models"
				idField = "slug"
			}
			if err := json.Unmarshal(payload[field], &models); err != nil {
				t.Fatal(err)
			}
			if len(models) != 2 {
				t.Fatalf("duplicate/unpublished OAuth models exposed: %s", w.Body.String())
			}
			if models[0][idField] != "codex2/"+identityModel || models[1][idField] != identityModel {
				t.Fatalf("public prefix separation lost: %s", w.Body.String())
			}
			for _, prefix := range []string{"codex", "codex2"} {
				if len(registry.GetGlobalRegistry().GetModelsForClient(f.auths[prefix].ID)) != 3 {
					t.Fatal("catalog mutated upstream routing registration")
				}
			}
			if len(f.executor.selected) != 0 {
				t.Fatal("model listing generated requests")
			}
		})
	}
}

func TestCatalogHidesDisabledReplacedAndRestrictedAccounts(t *testing.T) {
	for _, scenario := range []string{"disabled", "member-replaced", "key-policy"} {
		t.Run(scenario, func(t *testing.T) {
			f := newProfileFixture(t, false)
			if scenario == "key-policy" {
				digest := sha256.Sum256([]byte("issued-test-key"))
				raw, _ := json.Marshal(map[string]any{"keys": []map[string]any{{"id": "issued-fixture", "hash": hex.EncodeToString(digest[:]), "modelPatterns": []string{"codex2/*"}}}})
				if err := f.profile.Keys.Reload(raw); err != nil {
					t.Fatal(err)
				}
			} else {
				auth, _ := f.manager.GetByID("codex-fixture.json")
				if scenario == "disabled" {
					auth.Disabled = true
					auth.Status = coreauth.StatusDisabled
				} else {
					auth.Metadata["email"] = "replacement@example.invalid"
				}
				if _, err := f.manager.Update(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.Header.Set("Authorization", "Bearer issued-test-key")
			w := httptest.NewRecorder()
			f.engine.ServeHTTP(w, r)
			var doc struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &doc) != nil || len(doc.Data) != 1 || doc.Data[0].ID != "codex2/"+identityModel {
				t.Fatalf("unsafe account remained in catalog: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestLegacyProfileOptionsMountOnUpstreamServerWithoutInnerKeyBypass(t *testing.T) {
	f := newProfileFixture(t, false)
	access := sdkaccess.NewManager()
	cfg := &config.Config{Host: "127.0.0.1", CommercialMode: true, MaxRetryCredentials: 2}
	cfg.APIKeys = []string{"inner-test-key"}
	cfg.RemoteManagement.DisableControlPanel = true
	server := api.NewServer(cfg, f.manager, access, filepath.Join(t.TempDir(), "config.yaml"), f.profile.ServerOptions()...)
	for _, tc := range []struct {
		method, path, body, key string
		want                    int
	}{
		{http.MethodPost, "/v1/responses", `{"model":"gpt-identity-fixture","input":"fixture"}`, "admin-test-key", 200},
		{http.MethodPost, "/backend-api/codex/responses", `{"model":"codex2/gpt-identity-fixture","input":"fixture"}`, "issued-test-key", 200},
		{http.MethodPost, "/v1/responses", `{"model":"gpt-identity-fixture"}`, "inner-test-key", 401},
		{http.MethodGet, "/v1/models?client_version=0.162.1", "", "issued-test-key", 200},
		{http.MethodGet, "/v1/models", "", "inner-test-key", 401},
		{http.MethodPost, "/v1/responses/compact", `{"model":"gpt-identity-fixture"}`, "admin-test-key", 404},
		{http.MethodPost, "/backend-api/codex/responses/compact", `{"model":"gpt-identity-fixture"}`, "admin-test-key", 404},
		{http.MethodGet, "/v1/responses", "", "admin-test-key", 404},
		{http.MethodPost, "/v1/realtime/client_secrets", `{"model":"gpt-identity-fixture"}`, "admin-test-key", 404},
		{http.MethodPost, "/v1beta/interactions", `{"model":"gpt-identity-fixture"}`, "admin-test-key", 404},
		{http.MethodGet, "/v8/management/auth-files", "", "admin-test-key", 404},
		{http.MethodGet, "/codex/callback?state=fixture&code=fixture", "", "admin-test-key", 404},
		{http.MethodGet, "/health", "", "", 200},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer "+tc.key)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("actual server route %s failed %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	if strings.Join(f.executor.selected, ",") != "codex-fixture.json,codex2-fixture.json" {
		t.Fatalf("actual server account selection changed: %v", f.executor.selected)
	}
}

func (f *profileFixture) request(body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.RemoteAddr = "192.0.2.5:1234"
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("X-Auth-ID", "codex2-fixture.json")
	r.Header.Set("X-Forwarded-For", "198.51.100.5")
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, r)
	return w
}

func TestLegacyProfileHTTPPinsAccountAndReceiptThroughSDKContext(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			f := newProfileFixture(t, false)
			for i, model := range []string{identityModel, "codex2/" + identityModel, identityModel, "codex2/" + identityModel} {
				body := fmt.Sprintf(`{"model":%q,"input":"fixture","stream":%t,"auth_id":"codex2-fixture.json","metadata":{"pinned_auth_id":"codex2-fixture.json"}}`, model, stream)
				w := f.request(body, "issued-test-key")
				if w.Code != 200 {
					t.Fatalf("HTTP execution failed %d: %s", w.Code, w.Body.String())
				}
				want := "codex-fixture.json"
				if i%2 == 1 {
					want = "codex2-fixture.json"
				}
				if len(f.executor.selected) != i+1 || f.executor.selected[i] != want {
					t.Fatalf("account pin lost: %v", f.executor.selected)
				}
			}
			if err := f.executor.sink.Err(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(lines) != 4 {
				t.Fatalf("wrong receipt count %d", len(lines))
			}
			for i, line := range lines {
				var row billing.Row
				if json.Unmarshal([]byte(line), &row) != nil {
					t.Fatal("bad receipt")
				}
				want := "codex-member"
				if i%2 == 1 {
					want = "codex2-member"
				}
				if row.OAuthAccountID != want || row.KeyID != "issued-fixture" || row.ClientIP != "192.0.2.5" || row.ModelMismatch {
					t.Fatalf("wrong receipt identity: %+v", row)
				}
			}
		})
	}
}

func TestLegacyProfileHTTPRejectsBypassAmbiguityAndMemberReplacement(t *testing.T) {
	for _, name := range []string{"inner-key", "duplicate-model", "escaped-model", "unpublished", "member-replaced", "disabled-primary", "primary-429", "primary-unavailable"} {
		t.Run(name, func(t *testing.T) {
			f := newProfileFixture(t, name == "disabled-primary")
			body := `{"model":"gpt-identity-fixture","input":"fixture"}`
			key := "admin-test-key"
			switch name {
			case "inner-key":
				key = "inner-test-key"
			case "duplicate-model":
				body = `{"model":"gpt-identity-fixture","model":"codex2/gpt-identity-fixture"}`
			case "escaped-model":
				body = `{"model":"gpt-identity-fixture","\u006dodel":"codex2/gpt-identity-fixture"}`
			case "unpublished":
				body = `{"model":"gpt-unpublished-fixture"}`
			case "member-replaced":
				a, _ := f.manager.GetByID("codex-fixture.json")
				a.Metadata["email"] = "another@example.invalid"
				if _, err := f.manager.Update(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			case "primary-429":
				f.executor.failure["codex-fixture.json"] = 429
			case "primary-unavailable":
				f.manager.MarkResult(context.Background(), coreauth.Result{AuthID: "codex-fixture.json", Provider: "codex", Model: identityModel, RouteModel: "codex/" + identityModel, Error: &coreauth.Error{HTTPStatus: 429, Message: "fixture cooldown"}})
			}
			w := f.request(body, key)
			if w.Code < 400 {
				t.Fatalf("unsafe request accepted: %s", w.Body.String())
			}
			for _, id := range f.executor.selected {
				if id != "codex-fixture.json" {
					t.Fatal("failure crossed into another member")
				}
			}
			if name != "primary-429" && name != "primary-unavailable" && len(f.executor.selected) != 0 {
				t.Fatal("rejected request called upstream")
			}
			if name == "member-replaced" || name == "unpublished" {
				if f.executor.results != 0 {
					t.Fatal("local policy poisoned credential state")
				}
			}
			raw, _ := os.ReadFile(f.path)
			if len(raw) != 0 {
				t.Fatal("rejection generated billable receipt")
			}
		})
	}
}
