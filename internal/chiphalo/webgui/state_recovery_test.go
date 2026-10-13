package webgui

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConfirmedStateRecoveryPreservesTombstonesAndUnknownFields(t *testing.T) {
	s, _ := fixtureServer(t)
	state, err := s.state()
	if err != nil {
		t.Fatal(err)
	}
	state["providers"] = []any{map[string]any{"id": "keep", "prefix": "keep", "models": []any{}}, map[string]any{"id": "removed", "prefix": "removed", "models": []any{}}}
	if err := s.saveState(state); err != nil {
		t.Fatal(err)
	}
	state["providers"] = state["providers"].([]any)[:1]
	state["removedProviders"] = []any{map[string]any{"id": "removed", "removedAt": 100}}
	state["vendor_extension"] = map[string]any{"preserve": "opaque"}
	if err := s.saveState(state); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.options.Data, "state.json")
	if err := os.WriteFile(path, []byte("damaged-json"), 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := s.state()
	if err != nil || len(restored["providers"].([]any)) != 1 || restored["providers"].([]any)[0].(map[string]any)["id"] != "keep" || len(restored["removedProviders"].([]any)) != 1 || restored["vendor_extension"].(map[string]any)["preserve"] != "opaque" {
		t.Fatalf("confirmed recovery lost or resurrected subscriptions: %v", err)
	}
	metadata := restored["recovery"].(map[string]any)
	evidence, err := os.ReadFile(filepath.Join(s.options.Data, "state-recovery", metadata["damagedStateBackup"].(string)))
	if err != nil || string(evidence) != "damaged-json" {
		t.Fatal("damaged bytes were erased without private evidence")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if restored, err := s.state(); err != nil || len(restored["providers"].([]any)) != 1 {
		t.Fatal("missing file did not recover from confirmed latest inventory")
	}
}
func TestStatePublicationRestartBeforeAndAfterCommit(t *testing.T) {
	for _, phase := range []string{"before-publish", "after-publish"} {
		t.Run(phase, func(t *testing.T) {
			s, _ := fixtureServer(t)
			state, err := s.state()
			if err != nil {
				t.Fatal(err)
			}
			state["custom"] = "new-authorized-draft"
			s.stateFault = func(actual string) error {
				if phase == actual {
					return errors.New("synthetic interrupted publication")
				}
				return nil
			}
			if err := s.saveState(state); err == nil {
				t.Fatal("publication fault ignored")
			}
			if _, err := os.Stat(filepath.Join(s.options.Data, stateFence)); err != nil {
				t.Fatal("uncertain publication did not retain fence")
			}
			restarted, err := New(s.options)
			if err != nil {
				t.Fatal(err)
			}
			current, err := restarted.state()
			want := "keep"
			if phase == "after-publish" {
				want = "new-authorized-draft"
			}
			if err != nil || current["custom"] != want {
				t.Fatalf("restart selected the wrong publication: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(s.options.Data, stateFence)); !os.IsNotExist(err) {
				t.Fatal("resolved state fence remained")
			}
			if err := os.WriteFile(filepath.Join(s.options.Data, "state.json"), []byte("broken"), 0600); err != nil {
				t.Fatal(err)
			}
			current, err = restarted.state()
			if err != nil || current["custom"] != want {
				t.Fatal("later recovery restored an older preimage")
			}
		})
	}
}
func TestStateRecoveryNeverOverwritesValidExternalEditOrBadJournal(t *testing.T) {
	s, _ := fixtureServer(t)
	state, err := s.state()
	if err != nil {
		t.Fatal(err)
	}
	state["custom"] = "external-edit"
	path := filepath.Join(s.options.Data, "state.json")
	if err := writeJSON(path, state); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err := s.state(); err == nil {
		t.Fatal("untracked valid edit was silently blessed")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("external edit overwritten by automatic recovery")
	}
	if err := os.WriteFile(filepath.Join(s.options.Data, stateFence), []byte(`{"version":1,"image":"../../outside","after":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.state(); err == nil {
		t.Fatal("malformed publication journal accepted")
	}
	after, _ = os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("malformed journal caused state reset")
	}
}
func TestDamagedCheckpointAndInitialCorruptionFailClosed(t *testing.T) {
	s, _ := fixtureServer(t)
	path := filepath.Join(s.options.Data, "state.json")
	if err := os.WriteFile(path, []byte("initial-corruption"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.state(); err == nil {
		t.Fatal("initial corruption guessed an inventory without a confirmed state")
	}
	if raw, _ := os.ReadFile(path); string(raw) != "initial-corruption" {
		t.Fatal("initial state was overwritten")
	}
	s, _ = fixtureServer(t)
	if _, err := s.state(); err != nil {
		t.Fatal(err)
	}
	var record stateCheckpoint
	if err := readJSON(filepath.Join(s.options.Data, stateManifest), 4096, true, &record); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.options.Data, "state-recovery", record.Image), []byte("corrupt-checkpoint"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.state(); err == nil {
		t.Fatal("damaged recovery image was trusted")
	}
}
