package durableauth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type testStore struct {
	save func(context.Context, *coreauth.Auth) (string, error)
}

func (s *testStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	return s.save(ctx, auth)
}
func (*testStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (*testStore) Delete(context.Context, string) error           { return nil }

func privateRoots(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	for _, name := range []string{"auth", "health"} {
		if err := os.Mkdir(filepath.Join(base, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(base, "auth"), filepath.Join(base, "health")
}

func TestReadOnlyStoreCannotSaveDeleteOrRetarget(t *testing.T) {
	dir, health := privateRoots(t)
	called := false
	inner := &testStore{save: func(context.Context, *coreauth.Auth) (string, error) { called = true; return "", nil }}
	s, err := Open(dir, health, inner, true)
	if err != nil {
		t.Fatal(err)
	}
	s.SetBaseDir(filepath.Join(dir, "other"))
	if s.AuthDir() != dir {
		t.Fatal("YAML redirected the host's private auth root")
	}
	if _, err := s.Save(context.Background(), &coreauth.Auth{ID: "test.json", Provider: "codex"}); err != ErrReadOnly || called {
		t.Fatal("shadow wrote a copied credential")
	}
	if err := s.Delete(context.Background(), "test.json"); err != ErrReadOnly || s.Err() != nil {
		t.Fatal("shadow delete or unexpected health state")
	}
}
