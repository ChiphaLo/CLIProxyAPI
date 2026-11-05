package workbuddycli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

// ExportAccess publishes an access-only projection for a separate CLI UID. The
// original plugin remains the sole refresh-token owner. No credential is logged.
func ExportAccess(source, destination, expectedUID string) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(destination) || source == destination || expectedUID == "" {
		return errors.New("access projection requires fixed paths and an account identity")
	}
	failed := func() error {
		_ = os.Remove(destination)
		return errors.New("WorkBuddy credential projection unavailable")
	}
	raw, err := privatefile.Read(source, 128<<10, true)
	if err != nil {
		return failed()
	}
	var credential struct {
		Type        string `json:"type"`
		Domain      string `json:"domain"`
		UID         string `json:"uid"`
		AccessToken string `json:"accessToken"`
		ExpiresAt   int64  `json:"expiresAt"`
		Disabled    bool   `json:"disabled"`
	}
	if privatefile.Decode(raw, &credential) != nil || credential.Type != "codebuddy" || credential.UID != expectedUID || credential.Disabled || credential.Domain != "www.workbuddy.ai" && credential.Domain != "workbuddy.ai" || credential.AccessToken == "" || strings.ContainsAny(credential.AccessToken, "\x00\r\n") || credential.ExpiresAt <= time.Now().Unix() {
		return failed()
	}
	projection, err := json.Marshal(map[string]any{"accessToken": credential.AccessToken, "uid": credential.UID, "expiresAt": credential.ExpiresAt})
	if err != nil {
		return failed()
	}
	if old, readErr := privatefile.Read(destination, 128<<10, true); readErr == nil && bytes.Equal(old, projection) {
		return nil
	}
	parent := filepath.Dir(destination)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm()&0027 != 0 {
		return errors.New("access projection directory is not private")
	}
	temp, err := os.CreateTemp(parent, ".access-")
	if err != nil {
		return errors.New("cannot stage access projection")
	}
	defer func() { _ = temp.Close(); _ = os.Remove(temp.Name()) }()
	if temp.Chmod(0640) != nil {
		return errors.New("cannot protect access projection")
	}
	if _, err = temp.Write(projection); err != nil {
		return errors.New("cannot write access projection")
	}
	if temp.Sync() != nil || temp.Close() != nil {
		return errors.New("cannot synchronize access projection")
	}
	if os.Rename(temp.Name(), destination) != nil {
		return errors.New("cannot publish access projection")
	}
	if runtime.GOOS != "windows" {
		dir, errOpen := os.Open(parent)
		if errOpen != nil {
			return errors.New("cannot open access projection directory")
		}
		defer func() { _ = dir.Close() }()
		if dir.Sync() != nil {
			return errors.New("cannot synchronize access projection directory")
		}
	}
	return nil
}

func RunAccessExporter(ctx context.Context, source, destination, expectedUID string) error {
	if err := ExportAccess(source, destination, expectedUID); err != nil {
		return err
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := ExportAccess(source, destination, expectedUID); err != nil {
				return err
			}
		}
	}
}
