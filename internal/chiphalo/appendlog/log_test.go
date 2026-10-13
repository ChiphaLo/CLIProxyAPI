package appendlog

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestPreservesHistoricalBytesModeAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	old := "{\"historical\":1}\n"
	if err := os.WriteFile(path, []byte(old), 0640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := l.Append([]byte("{\"new\":1}\n")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatal("opening altered legacy inode or permissions")
	}
	if runtime.GOOS == "windows" {
		t.Skip("open-file rename behavior requires the Linux deployment filesystem")
	}
	if err := os.Rename(path, path+".rotated"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"replaced\":1}\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := l.Append([]byte("{\"new\":2}\n")); err != nil {
		t.Fatal(err)
	}
	rotated, _ := os.ReadFile(path + ".rotated")
	active, _ := os.ReadFile(path)
	if string(rotated) != old+"{\"new\":1}\n" || string(active) != "{\"replaced\":1}\n{\"new\":2}\n" {
		t.Fatal("wrote into obsolete history or altered replacement data")
	}
}

func TestRefusesPartialHistoryAndUnsafeReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	if err := os.WriteFile(path, []byte("{\"partial\":"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("accepted incomplete history")
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if runtime.GOOS == "windows" {
		t.Skip("open-file rename behavior requires the Linux deployment filesystem")
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".old", path); err != nil {
		t.Fatal(err)
	}
	if err := l.Append([]byte("{}\n")); err == nil {
		t.Fatal("followed a replacement symlink")
	}
	old, _ := os.ReadFile(path + ".old")
	if len(old) != 0 {
		t.Fatal("unsafe append changed historical data")
	}
}

func TestConcurrentAppendAndIdempotentClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := l.Append([]byte(fmt.Sprintf("{\"n\":%d}\n", n))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Append([]byte("{}\n")); err == nil {
		t.Fatal("accepted append after close")
	}
	raw, _ := os.ReadFile(path)
	if strings.Count(string(raw), "\n") != 100 {
		t.Fatal("lost or duplicated records")
	}
}
