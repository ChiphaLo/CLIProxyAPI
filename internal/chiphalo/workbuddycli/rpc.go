// Package workbuddycli adapts the unmodified official client over ACP. It does
// not implement client attestation, forge transport identities, or execute tools.
package workbuddycli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

type frame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type connection struct {
	in               io.WriteCloser
	frames           chan frame
	done             chan struct{}
	cancel           context.CancelFunc
	nextID           int
	observe          func(string, json.RawMessage) error
	readErr          error
	denials          int
	sessionID        string
	handoffCancelled bool
}

func start(ctx context.Context, node, entry, dir string, args, env []string) (*connection, error) {
	processCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(processCtx, node, append([]string{entry}, args...)...)
	cmd.Dir, cmd.Env, cmd.Stderr = dir, env, io.Discard
	configureProcess(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open official client input: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = in.Close()
		return nil, fmt.Errorf("open official client output: %w", err)
	}
	if err = cmd.Start(); err != nil {
		cancel()
		_ = in.Close()
		return nil, fmt.Errorf("start official client: %w", err)
	}
	c := &connection{in: in, frames: make(chan frame, 64), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(c.done)
		defer close(c.frames)
		scan := bufio.NewScanner(out)
		scan.Buffer(make([]byte, 4096), 8<<20)
		for scan.Scan() {
			var f frame
			if json.Unmarshal(scan.Bytes(), &f) != nil {
				// Diagnostic lines are never forwarded or interpreted as model text.
				continue
			}
			select {
			case c.frames <- f:
			case <-processCtx.Done():
				_ = cmd.Wait()
				return
			}
		}
		c.readErr = scan.Err()
		if errWait := cmd.Wait(); errWait != nil && c.readErr == nil {
			c.readErr = errors.New("official client exited unsuccessfully")
		}
	}()
	return c, nil
}

func (c *connection) close() {
	c.cancel()
	_ = c.in.Close()
	<-c.done
}

func (c *connection) write(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.in.Write(append(raw, '\n'))
	return err
}

func (c *connection) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("write official client request: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case f, ok := <-c.frames:
			if !ok {
				return nil, errors.New("official client disconnected before completion")
			}
			if f.Method != "" {
				if len(f.ID) != 0 {
					// ACP is a protocol peer, not an authority to run commands. Never
					// grant filesystem, terminal, approval, or delegate execution.
					var reply any
					if f.Method == "session/request_permission" {
						c.denials++
						reply = map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}}
					} else {
						reply = map[string]any{"jsonrpc": "2.0", "id": f.ID, "error": map[string]any{"code": -32601, "message": "Gateway delegates execution exclusively to its downstream caller"}}
					}
					if err := c.write(reply); err != nil {
						return nil, err
					}
					if f.Method == "session/request_permission" && c.sessionID != "" && !c.handoffCancelled {
						// The official client's parallel approval queue can remain pending
						// after a denial. Standard ACP cancellation settles the turn and
						// publishes final usage without approving any client operation.
						if err := c.write(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": c.sessionID}}); err != nil {
							return nil, err
						}
						c.handoffCancelled = true
					}
				} else if c.observe != nil {
					if err := c.observe(f.Method, f.Params); err != nil {
						return nil, err
					}
				}
				continue
			}
			if string(f.ID) != fmt.Sprint(id) {
				return nil, errors.New("official client returned an unmatched RPC response")
			}
			if len(f.Error) != 0 && string(f.Error) != "null" {
				// Error payloads can contain credentials, prompts, and local paths.
				return nil, fmt.Errorf("official client rejected ACP operation %s", method)
			}
			return f.Result, nil
		}
	}
}
