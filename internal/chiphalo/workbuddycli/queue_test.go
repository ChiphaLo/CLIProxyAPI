package workbuddycli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func queueNotice(ch chan struct{}) func() error {
	var once sync.Once
	return func() error { once.Do(func() { close(ch) }); return nil }
}

func TestQueueWaitsForSlotAndReleasesReservation(t *testing.T) {
	s := fixtureServer(t)
	s.slot <- struct{}{}
	waiting, finished := make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- s.acquire(t.Context(), 100, queueNotice(waiting))
	}()
	<-waiting
	select {
	case err := <-finished:
		t.Fatalf("busy worker was not queued: %v", err)
	default:
	}
	<-s.slot
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if s.queued != 0 || s.queuedBytes != 0 || len(s.slot) != 1 {
		t.Fatal("queue reservation leaked or active slot not held")
	}
	<-s.slot
}

func TestQueueCancellationDoesNotStartAnotherClient(t *testing.T) {
	s := fixtureServer(t)
	s.slot <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	waiting, finished := make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- s.acquire(ctx, 100, queueNotice(waiting))
	}()
	<-waiting
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("queue ignored cancellation: %v", err)
	}
	if s.queued != 0 || s.queuedBytes != 0 || len(s.slot) != 1 {
		t.Fatal("cancellation changed the active request or leaked a reservation")
	}
	<-s.slot
}

func TestQueueBoundsCountAndPayloadMemory(t *testing.T) {
	s := fixtureServer(t)
	s.maxQueued = 1
	s.slot <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	waiting, finished := make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- s.acquire(ctx, 100, queueNotice(waiting))
	}()
	<-waiting
	if err := s.acquire(t.Context(), 1, nil); !errors.Is(err, errQueueFull) {
		t.Fatalf("queue count bound ignored: %v", err)
	}
	cancel()
	<-finished
	if err := s.acquire(t.Context(), maxQueuedPayloadBytes+1, nil); !errors.Is(err, errQueueFull) {
		t.Fatalf("queue payload memory bound ignored: %v", err)
	}
	if s.queued != 0 || s.queuedBytes != 0 {
		t.Fatal("rejected request changed queue reservations")
	}
	<-s.slot
}

func TestQueueWriteFailureAndPreCancelledRequestDoNotLeak(t *testing.T) {
	s := fixtureServer(t)
	s.slot <- struct{}{}
	failure := errors.New("downstream disconnected")
	if err := s.acquire(t.Context(), 100, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if s.queued != 0 || s.queuedBytes != 0 {
		t.Fatal("heartbeat failure leaked queue reservation")
	}
	<-s.slot
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.acquire(ctx, 1, nil); !errors.Is(err, context.Canceled) || len(s.slot) != 0 {
		t.Fatalf("pre-cancelled request acquired worker: %v", err)
	}
}

func TestQueueConfigurationValidation(t *testing.T) {
	for _, limit := range []int{-1, 33} {
		if _, err := NewServerWithQueue(Config{}, fixtureKey, limit); err == nil {
			t.Fatalf("invalid queue limit accepted: %d", limit)
		}
	}
}

func TestHTTPOverlappingStreamsWaitWithoutBusyAndCancelCleanly(t *testing.T) {
	s := fixtureServer(t)
	entered, release, cancelled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.generate = func(ctx context.Context, r Request, emit func(string) error) (*Result, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err := emit("OK"); err != nil {
			return nil, err
		}
		return &Result{Model: r.Model, Text: "OK", FinishReason: "stop", Usage: json.RawMessage(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`)}, nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeHTTP(w, r)
		if r.Header.Get("X-Fixture") == "cancel" {
			close(cancelled)
		}
	}))
	defer server.Close()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	request := func(ctx context.Context, label string) *http.Response {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"fixture"}],"stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+fixtureKey)
		r.Header.Set("X-Fixture", label)
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	first := request(t.Context(), "active")
	defer first.Body.Close()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	second := request(ctx, "cancel")
	line, err := bufio.NewReader(second.Body).ReadString('\n')
	if err != nil || second.StatusCode != 200 || !strings.HasPrefix(line, ": waiting") {
		t.Fatalf("queued stream did not send SSE heartbeat: %d %q %v", second.StatusCode, line, err)
	}
	cancel()
	_ = second.Body.Close()
	<-cancelled
	if calls.Load() != 1 {
		t.Fatal("cancelled waiter started an official client")
	}
	third := request(t.Context(), "wait")
	defer third.Body.Close()
	releaseOnce.Do(func() { close(release) })
	for _, response := range []*http.Response{first, third} {
		raw, errRead := io.ReadAll(response.Body)
		if errRead != nil || !strings.Contains(string(raw), `"content":"OK"`) || !strings.Contains(string(raw), "[DONE]") || strings.Contains(string(raw), "worker_busy") {
			t.Fatalf("overlapping stream failed: %s %v", raw, errRead)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected official client starts: %d", calls.Load())
	}
}
