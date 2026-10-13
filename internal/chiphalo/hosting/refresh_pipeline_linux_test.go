//go:build linux

package hosting

import (
	"context"
	"encoding/json"
	"errors"
	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/durableauth"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/refreshowner"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type syntheticRefresh struct {
	started, release chan struct{}
	calls            int
}

func (*syntheticRefresh) Identifier() string { return "codex" }
func (*syntheticRefresh) Execute(context.Context, *coreauth.Auth, exec.Request, exec.Options) (exec.Response, error) {
	return exec.Response{}, errors.New("no inference in ownership test")
}
func (*syntheticRefresh) ExecuteStream(context.Context, *coreauth.Auth, exec.Request, exec.Options) (*exec.StreamResult, error) {
	return nil, errors.New("no inference in ownership test")
}
func (*syntheticRefresh) CountTokens(context.Context, *coreauth.Auth, exec.Request, exec.Options) (exec.Response, error) {
	return exec.Response{}, errors.New("no inference in ownership test")
}
func (*syntheticRefresh) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("no network in ownership test")
}
func (e *syntheticRefresh) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	e.calls++
	close(e.started)
	<-e.release
	b := a.Clone()
	b.Metadata["access_token"] = "synthetic-generation-two"
	b.Metadata["refresh_token"] = "synthetic-refresh-two"
	b.CodexRefreshPatch = codexauth.NewCredentialRefreshPatch(a.Metadata, b.Metadata, true, false)
	return b, nil
}

func TestHostedRefreshHandoffDrainsSDKPersistenceAndReloadsLatestGeneration(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	health := filepath.Join(root, "owner")
	for _, path := range []string{authDir, health} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	initial := []byte(`{"type":"codex","prefix":"codex","email":"synthetic@example.invalid","account_id":"synthetic-workspace","access_token":"synthetic-generation-one","refresh_token":"synthetic-refresh-one","extension":"preserve"}`)
	if err := os.WriteFile(filepath.Join(authDir, "member.json"), initial, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := refreshowner.Open(health)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close(context.Background()) }()
	openManager := func(lease *refreshowner.Lease) *coreauth.Manager {
		inner := sdkauth.NewFileTokenStore()
		inner.SetBaseDir(authDir)
		store, err := durableauth.Open(authDir, health, inner, false)
		if err != nil {
			t.Fatal(err)
		}
		manager := coreauth.NewManager(store, nil, nil)
		if err := manager.SetCredentialRefreshGuard(lease.Begin); err != nil {
			t.Fatal(err)
		}
		if err := manager.Load(t.Context()); err != nil {
			t.Fatal(err)
		}
		return manager
	}
	manager := openManager(first)
	e := &syntheticRefresh{started: make(chan struct{}), release: make(chan struct{})}
	manager.RegisterExecutor(e)
	done := make(chan error, 1)
	go func() { _, err := manager.ForceRefreshAuth(context.Background(), "member.json"); done <- err }()
	<-e.started
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := first.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("active persistence was not retained on canceled drain", err)
	}
	if next, err := refreshowner.Open(health); err == nil {
		_ = next.Close(context.Background())
		t.Fatal("second process could claim live refresh ownership")
	}
	close(e.release)
	if err := <-done; err != nil {
		t.Fatal("SDK refresh/persist failed", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	next, err := refreshowner.Open(health)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Close(context.Background()) }()
	loaded := openManager(next)
	auth, ok := loaded.GetByID("member.json")
	// FileTokenStore supplies persisted metadata; the SDK's file watcher owns
	// prefix synthesis. Do not require a second independent prefix parser here.
	if !ok || auth.Metadata["access_token"] != "synthetic-generation-two" || auth.Metadata["refresh_token"] != "synthetic-refresh-two" || auth.Metadata["prefix"] != "codex" {
		t.Fatalf("new owner freshness/identity mismatch: found=%t access=%t refresh=%t prefix=%t", ok, ok && auth.Metadata["access_token"] == "synthetic-generation-two", ok && auth.Metadata["refresh_token"] == "synthetic-refresh-two", ok && auth.Metadata["prefix"] == "codex")
	}
	raw, err := os.ReadFile(filepath.Join(authDir, "member.json"))
	var disk map[string]any
	_ = json.Unmarshal(raw, &disk)
	if err != nil || disk["extension"] != "preserve" || e.calls != 1 {
		t.Fatal("ownership handoff altered business metadata or refreshed twice")
	}
	if _, err := manager.ForceRefreshAuth(t.Context(), "member.json"); err == nil || e.calls != 1 {
		t.Fatal("drained old manager could refresh again")
	}
}
