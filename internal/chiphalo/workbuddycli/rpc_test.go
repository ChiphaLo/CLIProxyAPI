package workbuddycli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type recordingInput struct{ bytes.Buffer }

func (*recordingInput) Close() error { return nil }

func TestRPCDeniesEveryClientSideExecutionRequest(t *testing.T) {
	for _, method := range []string{"session/request_permission", "terminal/create", "fs/read_text_file", "fs/write_text_file", "_codebuddy.ai/delegateTool"} {
		t.Run(method, func(t *testing.T) {
			input := &recordingInput{}
			c := &connection{in: input, frames: make(chan frame, 2)}
			c.frames <- frame{ID: json.RawMessage(`99`), Method: method, Params: json.RawMessage(`{"untrusted":"payload"}`)}
			c.frames <- frame{ID: json.RawMessage(`1`), Result: json.RawMessage(`{}`)}
			if _, err := c.call(context.Background(), "fixture", map[string]any{}); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(input.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("unexpected protocol output: %s", input.String())
			}
			if method == "session/request_permission" {
				if !strings.Contains(lines[1], `"outcome":"cancelled"`) || c.denials != 1 {
					t.Fatal("granted tool execution permission")
				}
			} else if !strings.Contains(lines[1], `"code":-32601`) {
				t.Fatal("accepted a client-side operation")
			}
		})
	}
}

func TestRPCRejectsOutOfOrderResponseAndRedactsPeerErrors(t *testing.T) {
	for _, response := range []frame{{ID: json.RawMessage(`2`), Result: json.RawMessage(`{}`)}, {ID: json.RawMessage(`1`), Error: json.RawMessage(`{"message":"credential-secret-fixture"}`)}} {
		c := &connection{in: &recordingInput{}, frames: make(chan frame, 1)}
		c.frames <- response
		_, err := c.call(context.Background(), "fixture", map[string]any{})
		if err == nil || strings.Contains(err.Error(), "credential-secret-fixture") {
			t.Fatalf("unsafe peer error: %v", err)
		}
	}
}

func TestRPCCancellationDoesNotWaitForPeer(t *testing.T) {
	c := &connection{in: &recordingInput{}, frames: make(chan frame)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.call(ctx, "fixture", map[string]any{}); err != context.Canceled {
		t.Fatalf("cancel failed: %v", err)
	}
}

func TestPermissionDenialCancelsACPOnceAndDrainsParallelBatch(t *testing.T) {
	input := &recordingInput{}
	c := &connection{in: input, frames: make(chan frame, 5), sessionID: "isolated-session"}
	c.frames <- frame{ID: json.RawMessage(`81`), Method: "session/request_permission"}
	c.frames <- frame{Method: "session/update", Params: json.RawMessage(`{"second_tool":"complete"}`)}
	c.frames <- frame{ID: json.RawMessage(`82`), Method: "session/request_permission"}
	c.frames <- frame{ID: json.RawMessage(`1`), Result: json.RawMessage(`{"stopReason":"cancelled"}`)}
	observed := 0
	c.observe = func(string, json.RawMessage) error { observed++; return nil }
	if _, err := c.call(context.Background(), "session/prompt", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if observed != 1 || c.denials != 2 || !c.handoffCancelled || strings.Count(input.String(), `"method":"session/cancel"`) != 1 || strings.Count(input.String(), `"outcome":"cancelled"`) != 2 {
		t.Fatalf("parallel denial did not safely drain: %s", input.String())
	}
}
