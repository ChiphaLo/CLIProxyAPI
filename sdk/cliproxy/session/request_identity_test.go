package session

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestSelectiveIdentityAllProtocolsMatchesCanonical(t *testing.T) {
	cases := []struct {
		format sdktranslator.Format
		body   string
	}{
		{sdktranslator.FormatClaude, `{"system":[{"type":"text","text":"<system>&规则"}],"messages":[{"role":"assistant","content":"irrelevant"},{"role":"developer","content":"lead"},{"role":"user","content":[]},{"role":"user","content":[{"type":"text","text":"<>&\u2028"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},{"role":"user","content":"later"}]}`},
		{sdktranslator.FormatClaude, `{"system":"old","system":"new","messages":[{"role":"assistant","role":"user","content":"old","content":"last"}]}`},
		{sdktranslator.FormatOpenAI, `{"system":"ignored","messages":[{"role":"system","content":"rules"},{"role":"user","content":"question"}]}`},
		{sdktranslator.FormatGemini, `{"systemInstruction":{"parts":[{"text":"rules"}]},"cachedContent":"cache/a","contents":[{"role":"model","parts":[{"text":"ignored"}]},{"role":"user","parts":[{"text":"问"},{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]}]}`},
		{sdktranslator.FormatGemini, `{"cachedContent":"","cached_content":"ignored","contents":[{"role":"user","content":"primary","parts":[{"text":"secondary"}]}]}`},
		{sdktranslator.FormatGemini, `{"contents":[{"role":"user","text":"direct"}]}`},
		{sdktranslator.FormatAntigravity, `{"request":{"system_instruction":{"parts":[{"text":"rules"}]},"cached_content":"cache/b","contents":[{"role":"user","parts":[{"text":"question"}]}]}}`},
	}
	for _, c := range cases {
		var body map[string]any
		if err := json.Unmarshal([]byte(c.body), &body); err != nil {
			t.Fatal(err)
		}
		root := canonicalRoot{Version: identityVersion, Format: c.format.String(), CallerScope: "scope"}
		switch c.format {
		case sdktranslator.FormatClaude:
			root.Instructions, root.User = messagesRoot(body, true)
		case sdktranslator.FormatGemini, sdktranslator.FormatAntigravity:
			root.Instructions, root.User = geminiRoot(body)
			if req, ok := body["request"].(map[string]any); ok {
				body = req
			}
			root.Resource = stringField(body, "cachedContent", "cached_content")
		default:
			root.Instructions, root.User = messagesRoot(body, false)
		}
		if got, want := DeriveID(c.format, []byte(c.body), "scope"), legacyRootHash(t, root); got != want {
			t.Fatalf("%s identity changed: %s != %s", c.format, got, want)
		}
	}
}

func TestForwardIdentityUsesFinalInstructionsAndDestinationScope(t *testing.T) {
	body := []byte(`{"instructions":"original","input":[{"role":"user","content":"` + strings.Repeat("data ", 2000) + `"}]}`)
	_, opts := Enrich(coreexecutor.Request{Model: "claude-opus-5-5", Payload: body}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: "source"}})
	final := []byte(strings.Replace(string(body), "original", "final instructions", 1))
	got := ForwardDerivedID(final, sdktranslator.FormatOpenAIResponse, "target", opts.Metadata)
	if got != DeriveID(sdktranslator.FormatOpenAIResponse, final, "target") {
		t.Fatal("destination identity changed")
	}
	if got == DerivedID(opts.Metadata) {
		t.Fatal("caller namespaces merged")
	}
	changed := []byte(strings.Replace(string(final), "data ", "other ", 1))
	if ForwardDerivedID(changed, sdktranslator.FormatOpenAIResponse, "target", opts.Metadata) != DeriveID(sdktranslator.FormatOpenAIResponse, changed, "target") {
		t.Fatal("stale user content reused")
	}
}

func TestCanonicalUserCacheIsLimitedToNativeClaudeForwarding(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"` + strings.Repeat("large text ", 1000) + `"}]}`)
	for _, model := range []string{"gpt-6.1-sol", "deepseek-v4.1-flash", "claude-opus-5-5", "claude/claude-opus-5-5"} {
		_, opts := Enrich(coreexecutor.Request{Model: model, Payload: body}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
		identity := requestIdentity(body, opts.Metadata)
		if identity == nil || (len(identity.userJSON) > 0) != strings.Contains(model, "claude-") {
			t.Fatalf("unexpected canonical media cache for %s", model)
		}
		if DerivedID(opts.Metadata) != DeriveID(sdktranslator.FormatOpenAIResponse, body, "") {
			t.Fatalf("canonical identity changed for %s", model)
		}
		if ForwardDerivedID(body, sdktranslator.FormatOpenAIResponse, "destination", opts.Metadata) != DeriveID(sdktranslator.FormatOpenAIResponse, body, "destination") {
			t.Fatalf("uncached forwarding identity changed for %s", model)
		}
	}
}

func TestBridgeRejectsTamperingAndConsumesHeader(t *testing.T) {
	now := time.Unix(1800000000, 0)
	body := []byte(`{"input":"hello"}`)
	id := identityPrefix + strings.Repeat("a", 64)
	for _, kind := range []string{"valid", "wrong-key", "changed-body", "expired", "future", "malformed", "duplicate"} {
		h := http.Header{}
		h.Set(BridgeHeader, SignBridge("secret", body, id, now))
		key := "secret"
		input := body
		at := now
		switch kind {
		case "wrong-key":
			key = "other"
		case "changed-body":
			input = []byte(`{}`)
		case "expired":
			at = now.Add(121 * time.Second)
		case "future":
			at = now.Add(-11 * time.Second)
		case "malformed":
			h.Set(BridgeHeader, "v1.invalid")
		case "duplicate":
			h.Add(BridgeHeader, h.Get(BridgeHeader))
		}
		got := VerifyBridge(h, key, input, at)
		if (got == id) != (kind == "valid") {
			t.Fatalf("bridge accepted invalid claim: %s", kind)
		}
		if h.Get(BridgeHeader) != "" {
			t.Fatal("private header leaked")
		}
	}
}

func TestForwardExplicitSessionNeedsNoDerivedClaim(t *testing.T) {
	for _, body := range []string{`{"session_id":"session","input":"hello"}`, `{"prompt_cache_key":"cache","input":"hello"}`, `{"metadata":{"user_id":"opaque"},"input":"hello"}`} {
		if ForwardDerivedID([]byte(body), sdktranslator.FormatOpenAIResponse, "scope", nil) != "" {
			t.Fatal("explicit identity was unnecessarily derived")
		}
	}
}
