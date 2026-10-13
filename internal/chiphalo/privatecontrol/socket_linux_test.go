//go:build linux

package privatecontrol

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuthenticatedPrivateSocket(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "control.sock")
	l, err := Listen(path, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "private") })}
	go func() { _ = server.Serve(l) }()
	defer func() { _ = server.Close() }()
	client := Client(path, uint32(os.Geteuid()))
	response, err := client.Get("http://private/ready")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(raw) != "private" {
		t.Fatal("wrong response")
	}
	if _, err := Listen(path, uint32(os.Geteuid())); err == nil {
		t.Fatal("overwrote live listener")
	}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://private/ready", nil)
	if _, err := Client(path, uint32(os.Geteuid())+1).Do(request); err == nil {
		t.Fatal("accepted wrong server UID")
	}
}

func TestUnauthorizedControlPeer(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	path := filepath.Join(dir, "control.sock")
	l, err := Listen(path, uint32(os.Geteuid())+1)
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 1)
	s := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called <- struct{}{} })}
	go func() { _ = s.Serve(l) }()
	defer func() { _ = s.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://private/ready", nil)
	if _, err := Client(path, uint32(os.Geteuid())).Do(r); err == nil {
		t.Fatal("accepted unauthorized peer")
	}
	select {
	case <-called:
		t.Fatal("handler reached")
	default:
	}
}

func TestUnsafeControlParent(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0755)
	if _, err := Listen(filepath.Join(dir, "control.sock"), uint32(os.Geteuid())); err == nil {
		t.Fatal("accepted public parent")
	}
	_ = os.Chmod(dir, 0700)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(alias, "control.sock"), uint32(os.Geteuid())); err == nil {
		t.Fatal("accepted symlink parent")
	}
}
