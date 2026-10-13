package auth

import (
	"context"
	"reflect"
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
