package privatefile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadBoundedRegularPrivateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if raw, err := Read(path, 11, true); err != nil || string(raw) != `{"ok":true}` {
		t.Fatalf("private input unreadable: %v", err)
	}
	for _, limit := range []int64{0, -1, 10} {
		if _, err := Read(path, limit, true); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	for _, other := range []string{dir, filepath.Join(dir, "missing")} {
		if _, err := Read(other, 1024, true); err == nil {
			t.Fatal("non-file accepted")
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err == nil {
		if _, err := Read(link, 1024, false); err == nil {
			t.Fatal("symlink accepted")
		}
	} else if runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, mode := range []os.FileMode{0600, 0640} {
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(path, 1024, true); err != nil {
				t.Fatal(err)
			}
		}
		for _, mode := range []os.FileMode{0644, 0660, 0604} {
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(path, 1024, true); err == nil {
				t.Fatal("unsafe secret permissions accepted")
			}
		}
	}
}

func TestDecodeRejectsMalformedAndTrailingWithoutEchoingSecrets(t *testing.T) {
	for _, raw := range []string{`{"secret":"private-value"} {}`, `{"secret":"private-value"`, `{"secret":"private-value"} garbage`} {
		var value map[string]any
		if err := Decode([]byte(raw), &value); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatal("invalid input accepted or disclosed")
		}
	}
	var value map[string]any
	if err := Decode([]byte("{\"ok\":true}\n "), &value); err != nil {
		t.Fatal(err)
	}
}
