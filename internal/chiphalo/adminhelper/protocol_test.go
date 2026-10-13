package adminhelper

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStrictControlEnvelope(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{} {}`, `{"op":"status","op":"create_api_key","request_id":"test_1234"}`,
		`{"op":"status","request_id":"short"}`, `{"op":"status","request_id":"test_1234","exec":"shell"}`,
		`{"op":"unknown-secret-operation","request_id":"test_1234"}`,
		`{"op":"status","request_id":"test_1234","payload":null}`,
		`{"op":"status","request_id":"test_1234","payload":{"enabled":false,"enabled":true}}`,
	} {
		if _, _, _, err := parse([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid envelope: %s", raw)
		}
	}
	op, id, payload, err := parse([]byte(`{"op":"status","request_id":"test_1234","payload":{}}`))
	if err != nil || op != "status" || id != "test_1234" || len(payload) != 0 {
		t.Fatal("valid legacy envelope rejected")
	}
}

func TestResponseBoundIsCompleteJSON(t *testing.T) {
	raw := encode(Result{"ok": true, "data": strings.Repeat("x", MaxResponse)})
	var result Result
	if len(raw) > MaxResponse || raw[len(raw)-1] != '\n' || json.Unmarshal(raw, &result) != nil || result["ok"] != false {
		t.Fatal("oversized response truncated or successful")
	}
}

func TestScopePatchesClearLegacyAliases(t *testing.T) {
	entry := map[string]json.RawMessage{"model_patterns": set([]string{"restricted/*"}), "provider_prefixes": set([]string{"old"}), "future": set("keep")}
	if err := patchKey(entry, map[string]json.RawMessage{"modelPatterns": set([]string{}), "providerPrefixes": set(nil)}); err != nil {
		t.Fatal(err)
	}
	if entry["model_patterns"] != nil || entry["provider_prefixes"] != nil || string(entry["future"]) != `"keep"` {
		t.Fatal("legacy scope resurrected or unknown metadata lost")
	}
}

func TestInvalidPolicyDoesNotCoerce(t *testing.T) {
	for _, payload := range []map[string]json.RawMessage{
		{"enabled": set("false")}, {"enabled": set(nil)}, {"rpm": set(0)}, {"rpm": set(1.5)}, {"maxConcurrent": set(-1)},
		{"expiresAt": set(-1)}, {"modelPatterns": set([]any{"ok", 12})}, {"name": set(map[string]int{"x": 1})},
	} {
		if patchKey(map[string]json.RawMessage{}, payload) == nil {
			t.Fatal("invalid policy accepted")
		}
	}
	entry := map[string]json.RawMessage{}
	if err := patchKey(entry, map[string]json.RawMessage{"rpm": set(20000), "maxConcurrent": set(100)}); err != nil || string(entry["rpm"]) != "10000" || string(entry["maxConcurrent"]) != "64" {
		t.Fatal("legacy ceiling compatibility lost")
	}
}

func TestLegacyControlIdentitiesRemainStrict(t *testing.T) {
	for _, id := range []string{"UpperCase", "_leading", strings.Repeat("a", 65), "../escape", "prefix/name"} {
		if identity.MatchString(id) {
			t.Fatal("control identity widened beyond the existing GUI contract")
		}
	}
	for _, id := range []string{"key_123", "provider-1", "0-5"} {
		if !identity.MatchString(id) {
			t.Fatal("legacy identity rejected")
		}
	}
}
