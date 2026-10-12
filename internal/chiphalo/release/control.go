package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
)

type Change struct {
	Action           string `json:"action"`
	ID               string `json:"release_id,omitempty"`
	ExpectedRevision uint64 `json:"expected_revision"`
}

type Reply struct {
	State State  `json:"state"`
	Error string `json:"error,omitempty"`
}

// ControlHandler is deliberately not mounted on the data-plane listener.
// Only an owner-only Unix socket may expose it.
func (c *Controller) ControlHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/state" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(Reply{State: c.State()})
			return
		}
		if r.URL.Path != "/change" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
		var change Change
		if err != nil || decode(raw, &change) != nil || (change.Action != "activate" && change.Action != "rollback") || (change.Action == "rollback" && change.ID != "") {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(Reply{State: c.State(), Error: "invalid release transaction"})
			return
		}
		var state State
		if change.Action == "rollback" {
			state, err = c.Rollback(r.Context(), change.ExpectedRevision)
		} else {
			state, err = c.Activate(r.Context(), change.ID, change.ExpectedRevision)
		}
		if err != nil {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(Reply{State: state, Error: err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(Reply{State: state})
	})
}

func SocketPath(root string) string { return filepath.Join(root, "control.sock") }

// ListenControl recovers only an owner-private, connection-refused stale socket
// while holding the root lock. It never deletes an unknown live listener.
func (c *Controller) ListenControl() (net.Listener, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("release controller is closed")
	}
	path := SocketPath(c.root)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 || requirePrivate(info) != nil {
			return nil, fmt.Errorf("existing control path is not an owner-private socket")
		}
		conn, errDial := net.Dial("unix", path)
		if errDial == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("a live control listener already exists")
		}
		if !errors.Is(errDial, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("cannot establish that the control socket is stale")
		}
		current, errStat := os.Lstat(path)
		if errStat != nil || !os.SameFile(info, current) {
			return nil, fmt.Errorf("control socket changed during stale recovery")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return ownerListener(listener), nil
}

// ControlClient does not use HTTP_PROXY and never dials a network endpoint.
func ControlClient(root string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", SocketPath(root))
		},
	}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}
