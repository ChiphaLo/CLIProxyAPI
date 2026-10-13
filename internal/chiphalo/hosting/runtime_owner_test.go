package hosting

import (
	"strings"
	"testing"
)

func TestRuntimeOwnerRejectsRollbackReentryAndChangedBundle(t *testing.T) {
	m := &Manifest{Mode: "production", ReleaseID: "v1", sourceSHA: strings.Repeat("a", 64)}
	for _, phase := range []string{"starting-go", "committed", "preparing-go", "draining", "legacy", "", "restored"} {
		t.Run(phase, func(t *testing.T) {
			err := validateRuntimeOwner(m, RuntimeOwner{Version: 1, ReleaseID: "v1", ManifestSHA: m.sourceSHA, Phase: phase})
			allowed := phase == "starting-go" || phase == "committed"
			if (err == nil) != allowed {
				t.Fatal("runtime ownership phase widened")
			}
		})
	}
	for _, owner := range []RuntimeOwner{{Version: 1, ReleaseID: "v2", ManifestSHA: m.sourceSHA, Phase: "committed"}, {Version: 1, ReleaseID: "v1", ManifestSHA: strings.Repeat("b", 64), Phase: "committed"}, {Version: 2, ReleaseID: "v1", ManifestSHA: m.sourceSHA, Phase: "committed"}} {
		if validateRuntimeOwner(m, owner) == nil {
			t.Fatal("changed runtime identity accepted")
		}
	}
	if ConfirmRuntimeOwner(&Manifest{Mode: "rehearsal"}) != nil {
		t.Fatal("shadow was promoted to a production writer")
	}
}
