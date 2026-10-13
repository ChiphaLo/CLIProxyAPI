package webgui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAdoptCurrentStatePreservesLegacyEditsAndByteIdentity(t *testing.T) {
	s, _ := fixtureServer(t)
	_, err := s.state()
	if err != nil {
		t.Fatal(err)
	}
	data := s.options.Data
	meta, _ := os.ReadFile(filepath.Join(data, stateManifest))
	raw := []byte("{\"providers\":[],\"managedPrefixes\":[],\"legacy_new_field\":\"current\"}\n")
	if err := os.WriteFile(filepath.Join(data, "state.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.state(); err == nil {
		t.Fatal("ordinary startup silently adopted external edit")
	}
	if err := AdoptCurrentState(data, stateDigest(raw), stateDigest(meta)); err != nil {
		t.Fatal(err)
	}
	now, _ := os.ReadFile(filepath.Join(data, "state.json"))
	if string(now) != string(raw) {
		t.Fatal("handoff rewrote current state")
	}
	state, err := s.state()
	if err != nil || state["legacy_new_field"] != "current" {
		t.Fatal("current state not adopted")
	}
	if err := AdoptCurrentState(data, stateDigest(raw), stateDigest(meta)); err != nil {
		t.Fatal("durable adoption not idempotent", err)
	}
}
func TestAdoptionRejectsFenceStaleCASAndCorruptCheckpoint(t *testing.T) {
	for _, kind := range []string{"fence", "stale-state", "stale-metadata", "corrupt-image", "invalid-state"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := fixtureServer(t)
			_, _ = s.state()
			data := s.options.Data
			meta, _ := os.ReadFile(filepath.Join(data, stateManifest))
			raw := []byte("{\"providers\":[],\"new\":true}\n")
			_ = os.WriteFile(filepath.Join(data, "state.json"), raw, 0600)
			stateSHA, metaSHA := stateDigest(raw), stateDigest(meta)
			switch kind {
			case "fence":
				_ = os.WriteFile(filepath.Join(data, stateFence), []byte("{}"), 0600)
			case "stale-state":
				stateSHA = stateDigest([]byte("old"))
			case "stale-metadata":
				metaSHA = stateDigest([]byte("old"))
			case "corrupt-image":
				var record stateCheckpoint
				_ = readJSON(filepath.Join(data, stateManifest), 4096, true, &record)
				_ = os.WriteFile(filepath.Join(data, "state-recovery", record.Image), []byte("broken"), 0600)
			case "invalid-state":
				raw = []byte("{}")
				_ = os.WriteFile(filepath.Join(data, "state.json"), raw, 0600)
				stateSHA = stateDigest(raw)
			}
			if err := AdoptCurrentState(data, stateSHA, metaSHA); err == nil {
				t.Fatal("unsafe adoption accepted")
			}
			current, _ := os.ReadFile(filepath.Join(data, "state.json"))
			if string(current) != string(raw) {
				t.Fatal("rejection rewrote business data")
			}
		})
	}
}
func TestAdoptionRejectsDuplicateStateFields(t *testing.T) {
	s, _ := fixtureServer(t)
	_, _ = s.state()
	meta, _ := os.ReadFile(filepath.Join(s.options.Data, stateManifest))
	raw := []byte("{\"providers\":[],\"providers\":[]}\n")
	_ = os.WriteFile(filepath.Join(s.options.Data, "state.json"), raw, 0600)
	if err := AdoptCurrentState(s.options.Data, stateDigest(raw), stateDigest(meta)); err == nil {
		t.Fatal("ambiguous current state adopted")
	}
}
