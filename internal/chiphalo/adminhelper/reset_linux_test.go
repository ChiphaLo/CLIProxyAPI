//go:build linux

package adminhelper

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestResetCreditReadOnlyIPCDoesNotHoldMutationGate(t *testing.T) {
	s := fixture(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.inventory = &HTTPBackend{private: true, origin: "http://private", client: &http.Client{Transport: probeRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/reset-credits" {
			t.Error("unexpected reset read-only IPC request")
		}
		close(entered)
		<-release
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"accounts":{}}`))}, nil
	})}}
	go func() {
		defer close(done)
		_ = s.dispatchContext(context.Background(), "oauth_reset_credits", nil)
	}()
	<-entered
	if value := s.dispatchContext(context.Background(), "oauth_reset_credits", nil); value["ok"] != false || value["error"] != "reset credit lookup busy" {
		t.Error("unbounded reset reads admitted")
	}
	select {
	case s.gate <- struct{}{}:
		<-s.gate
	default:
		t.Error("official GET monopolized mutation gate")
	}
	value := s.dispatchContext(context.Background(), "oauth_reset_credits", map[string]json.RawMessage{"url": set("https://different.example")})
	if value["ok"] != false {
		t.Error("caller-selected upstream reset URL allowed")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reset read did not complete")
	}
}
