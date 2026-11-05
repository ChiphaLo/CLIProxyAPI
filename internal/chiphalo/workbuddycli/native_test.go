package workbuddycli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeUpstreamPreservesPayloadAndOverlapsWithoutCLISlot(t *testing.T) {
	ready, release := make(chan struct{}, 2), make(chan struct{})
	want := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"fixture"}],"stream":true,"max_tokens":4096,"reasoning_effort":"medium"}`
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != want || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+fixtureKey {
			t.Errorf("native payload or authentication changed")
		}
		ready <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: native-fixture\n\ndata: [DONE]\n\n")
	}))
	defer native.Close()
	s := fixtureServer(t)
	if err := s.SetNativeUpstream(native.URL); err != nil {
		t.Fatal(err)
	}
	s.slot <- struct{}{} // Native requests must not enter the legacy CLI scheduler.
	done := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(want))
			r.Header.Set("Authorization", "Bearer "+fixtureKey)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			done <- w
		}()
	}
	<-ready
	<-ready
	close(release)
	for range 2 {
		w := <-done
		if w.Code != 200 || !strings.Contains(w.Body.String(), "native-fixture") || !strings.Contains(w.Body.String(), "[DONE]") {
			t.Fatalf("native stream failed: %d %s", w.Code, w.Body.String())
		}
	}
	if len(s.slot) != 1 || s.queued != 0 {
		t.Fatal("native forwarding changed the CLI queue")
	}
	<-s.slot
}

func TestNativeUpstreamRejectsNonLoopbackOrigins(t *testing.T) {
	for _, address := range []string{"https://127.0.0.1:18336", "http://example.com:18336", "http://127.0.0.1:18336/v1", "http://secret@127.0.0.1:18336", "http://127.0.0.1:18336?url=other"} {
		if err := fixtureServer(t).SetNativeUpstream(address); err == nil {
			t.Fatalf("unsafe native origin accepted: %s", address)
		}
	}
}
