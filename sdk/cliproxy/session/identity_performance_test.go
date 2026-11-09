package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func legacyRootHash(t *testing.T, root canonicalRoot) string {
	t.Helper()
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return identityPrefix + hex.EncodeToString(sum[:])
}

func TestStreamedRootHashPreservesCanonicalIdentity(t *testing.T) {
	for _, root := range []canonicalRoot{
		{},
		{Version: identityVersion, Format: "openai-response", CallerScope: "<scope>&", Instructions: []string{"中文\n\"\\\u2028\u2029"}, User: []canonicalPart{{Kind: "text", Value: "<b>á界</b>\n\t"}, {Kind: "image", MIME: "image/png", Value: "data:image/png;base64," + strings.Repeat("ABC+xyz/=", 100000)}}},
		{Version: identityVersion, Format: "gemini", Resource: "cache/test", User: []canonicalPart{{Kind: "json", Value: `{"x":3}`}, {Kind: "file", MIME: "application/pdf", Value: "AA=="}}},
	} {
		if got, want := hashRoot(root), legacyRootHash(t, root); got != want {
			t.Fatalf("canonical identity changed: %s != %s", got, want)
		}
	}
}

func TestSelectiveResponsesRootMatchesLegacyIdentity(t *testing.T) {
	fixtures := []string{
		`{"instructions":"top","input":"hello 世界"}`,
		`{"input":[null,{"role":"developer","content":[{"type":"input_text","text":"优先级\n规则"}]},{"role":"user","content":[]},{"role":"user","content":[{"type":"input_text","text":"<>&\u2028"},{"type":"input_image","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"input_file","file_data":"data:application/pdf;base64,AA=="}]},{"role":"user","content":"later"}]}`,
		`{"instructions":"old","instructions":[{"text":"new"}],"input":[{"role":"assistant","role":"user","content":"first","content":"last"}]}`,
		`{"input":[{"role":"user","content":[{"other":{"x":1,"cache_control":{}}},true,3,null]}]}`,
	}
	for _, fixture := range fixtures {
		var body map[string]any
		if err := json.Unmarshal([]byte(fixture), &body); err != nil {
			t.Fatal(err)
		}
		instructions, user := responsesRoot(body)
		root := canonicalRoot{Version: identityVersion, Format: sdktranslator.FormatOpenAIResponse.String(), CallerScope: "scope", Instructions: instructions, User: user}
		got := DeriveID(sdktranslator.FormatOpenAIResponse, []byte(fixture), "scope")
		if got != legacyRootHash(t, root) {
			t.Fatalf("selective root changed identity for %s", fixture)
		}
	}
}
