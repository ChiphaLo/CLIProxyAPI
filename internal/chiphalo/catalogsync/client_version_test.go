package catalogsync

import (
	"strconv"
	"strings"
	"testing"
)

// The official catalog filters by minimal_client_version; an outdated default
// silently hides newly released models from discovery and review.
func TestDefaultClientVersionExposesCurrentModels(t *testing.T) {
	const gpt62Minimum = "0.166.0"
	if !versionPattern.MatchString(DefaultClientVersion) || ClientVersion("other-agent") != DefaultClientVersion {
		t.Fatal("default client version must be valid and used without a trusted Codex header")
	}
	parse := func(version string) [3]int {
		var out [3]int
		parts := strings.Split(version, ".")
		if len(parts) != 3 {
			t.Fatalf("invalid version %q", version)
		}
		for i, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil {
				t.Fatalf("invalid version %q", version)
			}
			out[i] = n
		}
		return out
	}
	got, minimum := parse(DefaultClientVersion), parse(gpt62Minimum)
	for i := range got {
		if got[i] > minimum[i] {
			return
		}
		if got[i] < minimum[i] {
			t.Fatalf("DefaultClientVersion %s hides models requiring %s", DefaultClientVersion, gpt62Minimum)
		}
	}
}
