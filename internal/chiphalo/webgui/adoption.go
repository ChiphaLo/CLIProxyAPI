package webgui

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

// AdoptCurrentState changes recovery metadata only, under an explicit stopped-
// writer deployment handoff. It never writes state.json or restores an image.
// Pending publications, corrupt checkpoints and stale CAS values fail closed.
func AdoptCurrentState(data, expectedState, expectedMetadata string) error {
	if !filepath.IsAbs(data) || filepath.Clean(data) != data || !stateDigestPattern.MatchString(expectedState) || expectedMetadata != "missing" && !stateDigestPattern.MatchString(expectedMetadata) {
		return errors.New("explicit current-state adoption identity required")
	}
	resolved, err := filepath.EvalSymlinks(data)
	if err != nil || resolved != data {
		return errors.New("real GUI data root required")
	}
	if _, err := os.Lstat(filepath.Join(data, stateFence)); !os.IsNotExist(err) {
		return errors.New("pending GUI publication must be recovered before ownership handoff")
	}
	s := &Server{options: Options{Data: data}}
	raw, err := readAdoptionState(data)
	if err != nil || stateDigest(raw) != expectedState {
		return errors.New("current GUI state adoption CAS conflict")
	}
	var before []byte
	var record stateCheckpoint
	meta := filepath.Join(data, stateManifest)
	if _, err := os.Lstat(meta); !os.IsNotExist(err) {
		before, err = privateStateMetadata(meta)
		if err != nil || readJSON(meta, 4096, true, &record) != nil {
			return errors.New("GUI recovery metadata unavailable")
		}
		if _, err := s.confirmedStateImage(record); err != nil {
			return err
		}
		// A durable metadata rename followed by a controller crash is idempotent.
		if record.Digest == expectedState {
			return nil
		}
	}
	currentMetadata := "missing"
	if before != nil {
		currentMetadata = stateDigest(before)
	}
	if currentMetadata != expectedMetadata {
		return errors.New("GUI recovery metadata adoption CAS conflict")
	}
	image, err := s.stateImage(raw)
	if err != nil {
		return err
	}
	current, err := readAdoptionState(data)
	if err != nil || stateDigest(current) != expectedState {
		return errors.New("GUI state changed during handoff")
	}
	after, err := stateBytes(stateCheckpoint{Version: 1, Digest: expectedState, Image: image})
	if err != nil {
		return err
	}
	return publishStateFile(meta, after, before, true)
}

// CheckReady reads existing documents and checks the actual private control
// chain. It does not call Reset, quota, upstream probes or configuration apply.
func (s *Server) CheckReady(ctx context.Context) error {
	s.mu.Lock()
	_, err := s.loadRecoverableState()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	value, err := s.options.Helper.Call(ctx, "status", nil)
	if err != nil || value["ok"] != true {
		return errors.New("GUI helper readiness unavailable")
	}
	status, ok := value["status"].(map[string]any)
	if !ok || status["gateway"] != "active" || status["configuration_ready"] != true {
		return errors.New("GUI configuration readiness unconfirmed")
	}
	return nil
}
func readAdoptionState(data string) ([]byte, error) {
	raw, err := privatefile.Read(filepath.Join(data, "state.json"), 4*1024*1024, false)
	if err != nil {
		return nil, err
	}
	if _, err := decodeState(raw); err != nil {
		return nil, err
	}
	return raw, nil
}
func privateStateMetadata(path string) ([]byte, error) { return privatefile.Read(path, 4096, true) }
