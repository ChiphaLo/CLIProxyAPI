//go:build linux

package adminhelper

import (
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/providerprobe"
)

func TestSavedUnpublishedKeyProbeSurvivesHelperRestart(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			s, calls := probeFixture(t)
			base := "https://new.example/v1"
			if protocol == "messages" {
				base = "https://new.example"
			}
			requireOK(t, call(s, "set_provider_secret", map[string]any{"provider_id": "saved-new", "secret": "fixture-secret", "base_url": base, "proxy_url": "socks5h://172.17.0.1:10808", "api_format": protocol}))
			raw, _ := os.ReadFile(s.paths.Vault)
			var doc map[string]json.RawMessage
			if json.Unmarshal(raw, &doc) != nil || string(doc["untouched"]) != `"old"` {
				t.Fatal("unrelated vault entry lost")
			}
			restarted, err := New(s.paths)
			if err != nil {
				t.Fatal(err)
			}
			restarted.inventory, restarted.probe = s.inventory, s.probe
			restarted.probe.Transport = func(in providerprobe.Input, _ []netip.Addr) (http.RoundTripper, error) {
				calls.Add(1)
				if in.Secret != "fixture-secret" || in.Base != base || in.Proxy != "socks5h://172.17.0.1:10808" || in.Format != protocol {
					t.Fatal("saved credential tuple changed")
				}
				return probeRoundTrip(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"saved-model"}]}`))}, nil
				}), nil
			}
			payload := map[string]any{"provider_id": "saved-new", "base_url": base, "proxy_policy": "cpa-egress", "proxy_url": "socks5h://172.17.0.1:10808", "api_format": protocol}
			requireOK(t, call(restarted, "probe_provider_models", payload))
			for field, value := range map[string]string{"base_url": "https://attacker.example/v1", "proxy_url": "socks5h://127.0.0.1:10808"} {
				bad := map[string]any{}
				for k, v := range payload {
					bad[k] = v
				}
				bad[field] = value
				if call(restarted, "probe_provider_models", bad)["ok"] != false {
					t.Fatal("saved key sent to a changed tuple")
				}
			}
			bad := map[string]any{}
			for k, v := range payload {
				bad[k] = v
			}
			bad["api_format"] = "messages"
			if protocol == "messages" {
				bad["api_format"] = "chat"
			}
			if call(restarted, "probe_provider_models", bad)["ok"] != false || calls.Load() != 1 {
				t.Fatal("saved key crossed protocol boundary")
			}
			after, _ := os.ReadFile(s.paths.Vault)
			if string(raw) != string(after) {
				t.Fatal("read-only probe changed credential database")
			}
		})
	}
}

func TestCredentialBindingRejectsUnsafeSaveAndTracksRotation(t *testing.T) {
	s, _ := probeFixture(t)
	for _, bad := range []map[string]any{
		{"provider_id": "new", "secret": "secret", "base_url": "http://127.0.0.1:8317", "api_format": "chat"},
		{"provider_id": "new", "secret": "secret", "proxy_url": "socks5h://172.17.0.1:10808"},
		{"provider_id": "new", "secret": "secret", "base_url": "https://new.example/v1", "api_format": "other"},
	} {
		before, _ := os.ReadFile(s.paths.Vault)
		if call(s, "set_provider_secret", bad)["ok"] != false {
			t.Fatal("invalid credential binding saved")
		}
		after, _ := os.ReadFile(s.paths.Vault)
		if string(before) != string(after) {
			t.Fatal("failed save mutated vault")
		}
	}
	setBound := func() {
		requireOK(t, call(s, "set_provider_secret", map[string]any{"provider_id": "new", "secret": "fixture-secret", "base_url": "https://new.example/v1", "api_format": "chat"}))
	}
	setBound()
	requireOK(t, call(s, "set_provider_secret", map[string]any{"provider_id": "new", "secret": "rotated-without-origin"}))
	raw, _ := os.ReadFile(s.paths.Vault)
	doc, _ := object(raw)
	bindings, _ := readProbeBindings(doc)
	if _, ok := bindings["new"]; ok {
		t.Fatal("unbound rotation kept stale binding")
	}
	setBound()
	requireOK(t, call(s, "delete_provider_secret", map[string]any{"provider_id": "new"}))
	raw, _ = os.ReadFile(s.paths.Vault)
	doc, _ = object(raw)
	bindings, _ = readProbeBindings(doc)
	if _, ok := bindings["new"]; ok {
		t.Fatal("deletion kept credential binding")
	}
}

func TestBoundProbeDoesNotRequirePublishedInventory(t *testing.T) {
	s, _ := probeFixture(t)
	requireOK(t, call(s, "set_provider_secret", map[string]any{"provider_id": "new", "secret": "fixture-secret", "base_url": "https://new.example/v1"}))
	s.inventory = nil
	requireOK(t, call(s, "probe_provider_models", map[string]any{"provider_id": "new", "base_url": "https://new.example/v1"}))
}
