package hosting

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostedManifestRejectsAmbiguousModesAndIncompleteAuthority(t *testing.T) {
	for _, raw := range []string{`{"version":1,"version":1}`, `{"version":1,"mode":"production","unknown":true}`, `{"version":1,"mode":"shadow"}`, `{"version":1,"mode":"rehearsal","gateway_uid":0}`, `{"version":1,"mode":"production","program_root":"/tmp/../opt"}`} {
		p := filepath.Join(t.TempDir(), "hosting.json")
		if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p, "cpa-gateway"); err == nil {
			t.Fatal("incomplete or ambiguous hosting authority accepted")
		}
	}
}
func TestHostedContractsDoNotPromoteRehearsalReadiness(t *testing.T) {
	m := &Manifest{Mode: "rehearsal", BusinessRoot: "/copy/opt/unified-ai-gateway", StateRoot: "/copy/state", RuntimeRoot: "/run/rehearsal"}
	if m.Production() || m.HealthContract() != "cpa-shadow-validation-v1" || filepath.ToSlash(m.Control()) != "/run/rehearsal/gateway/control.sock" || filepath.ToSlash(m.OAuthHealth()) != "/copy/state/oauth" {
		t.Fatal("rehearsal was promoted or paths escaped pinned roots")
	}
	if loopback("0.0.0.0:8317") || loopback("localhost:8317") || loopback("127.0.0.1:443") || !loopback("127.0.0.1:18317") {
		t.Fatal("hosting listener authority widened")
	}
}
