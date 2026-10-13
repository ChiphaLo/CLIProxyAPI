package auth

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestShadowRefreshOwnershipBlocksAutomaticUnauthorizedAndForce(t *testing.T) {
	m := NewManager(nil, nil, nil)
	e := &unauthorizedRefreshExecutor{id: "codex"}
	m.RegisterExecutor(e)
	auth := &Auth{ID: "shadow-refresh-fixture", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "private-fixture", "refresh_token": "refresh-fixture", "expired": time.Now().Add(-time.Hour).Format(time.RFC3339)}}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	before, _ := m.GetByID(auth.ID)
	m.DisableCredentialRefresh()
	m.StartAutoRefresh(context.Background(), time.Second)
	if m.refreshLoop != nil {
		t.Fatal("shadow scheduled a refresh worker")
	}
	m.refreshAuth(context.Background(), auth.ID)
	if _, err := m.ForceRefreshAuth(context.Background(), auth.ID); err == nil {
		t.Fatal("force refresh bypassed shadow ownership")
	}
	if _, ok := m.tryRefreshAfterUnauthorized(context.Background(), auth, &Error{HTTPStatus: 401}, false); ok {
		t.Fatal("401 consumed shared refresh credential")
	}
	after, _ := m.GetByID(auth.ID)
	if e.refreshCalls != 0 || !reflect.DeepEqual(before.Metadata, after.Metadata) {
		t.Fatal("shadow altered production-owned credential")
	}
}

func TestCredentialRefreshGuardDeniesAllPathsWithoutMutation(t *testing.T) {
	m := NewManager(nil, nil, nil)
	var calls atomic.Int32
	guard := func(context.Context) (func(), error) {
		calls.Add(1)
		return nil, errors.New("private ownership diagnostic must not escape")
	}
	if err := m.SetCredentialRefreshGuard(guard); err != nil {
		t.Fatal(err)
	}
	if err := m.SetCredentialRefreshGuard(guard); err == nil {
		t.Fatal("guard replaced at runtime")
	}
	e := &unauthorizedRefreshExecutor{id: "codex"}
	m.RegisterExecutor(e)
	auth := &Auth{ID: "owner-fixture", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "private-fixture", "refresh_token": "refresh-fixture"}}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	before, _ := m.GetByID(auth.ID)
	m.refreshAuth(context.Background(), auth.ID)
	if _, err := m.ForceRefreshAuth(context.Background(), auth.ID); err == nil || err.Error() != "credential refresh ownership unavailable" {
		t.Fatalf("force refresh did not fail closed: %v", err)
	}
	if _, ok := m.tryRefreshAfterUnauthorized(context.Background(), auth, &Error{HTTPStatus: 401}, false); ok {
		t.Fatal("401 bypassed ownership")
	}
	after, _ := m.GetByID(auth.ID)
	if calls.Load() != 3 || e.RefreshCalls() != 0 || !reflect.DeepEqual(before, after) {
		t.Fatal("ownership denial changed credential state or bypassed a refresh path")
	}
}

func TestCredentialRefreshGuardHoldsThroughPersistenceAndCancellation(t *testing.T) {
	for _, persistError := range []bool{false, true} {
		t.Run(map[bool]string{false: "persisted", true: "persist failure"}[persistError], func(t *testing.T) {
			var held atomic.Bool
			var releases atomic.Int32
			var saves atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &callbackAuthStore{save: func(auth *Auth) error {
				saves.Add(1)
				if !held.Load() || authAccessToken(auth) != "refreshed-access-token" {
					t.Error("ownership released before refreshed token persistence")
				}
				if persistError {
					return errors.New("synthetic persistence failure")
				}
				return nil
			}}
			m := NewManager(store, nil, nil)
			if err := m.SetCredentialRefreshGuard(func(context.Context) (func(), error) {
				held.Store(true)
				return func() { held.Store(false); releases.Add(1) }, nil
			}); err != nil {
				t.Fatal(err)
			}
			e := &unauthorizedRefreshExecutor{id: "codex", onRefresh: func() {
				if !held.Load() {
					t.Error("upstream refresh outside ownership barrier")
				}
				cancel()
			}}
			m.RegisterExecutor(e)
			if _, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: "owner-fixture", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "old", "refresh_token": "refresh-fixture"}}); err != nil {
				t.Fatal(err)
			}
			_, err := m.ForceRefreshAuth(ctx, "owner-fixture")
			// Upstream intentionally reports store failures non-fatally. The
			// ownership hook preserves that contract, not a durability guarantee.
			if err != nil || saves.Load() != 1 || releases.Load() != 1 || held.Load() {
				t.Fatalf("bad ownership completion: error=%v releases=%d held=%v", err, releases.Load(), held.Load())
			}
		})
	}
}

func TestCredentialRefreshGuardRequiresStartup(t *testing.T) {
	guard := func(context.Context) (func(), error) { return func() {}, nil }
	m := NewManager(nil, nil, nil)
	if err := m.SetCredentialRefreshGuard(nil); err == nil {
		t.Fatal("nil guard accepted")
	}
	if _, err := m.Register(context.Background(), &Auth{ID: "loaded", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetCredentialRefreshGuard(guard); err == nil {
		t.Fatal("guard installed after credential loading")
	}
}
