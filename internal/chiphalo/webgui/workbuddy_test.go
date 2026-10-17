package webgui

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

type workBuddyHelper struct{ calls int }

func (h *workBuddyHelper) Call(_ context.Context, op string, payload map[string]any) (map[string]any, error) {
	if op != "workbuddy_credits" || len(payload) != 0 {
		panic("unexpected mutating operation")
	}
	h.calls++
	return map[string]any{"ok": true, "accounts": []any{map[string]any{"prefix": "workbuddy-cn", "remaining_credits": 2195}}}, nil
}

func TestWorkBuddyCreditRouteRequiresLoginAndReadsOnly(t *testing.T) {
	s, _ := fixtureServer(t)
	h := &workBuddyHelper{}
	s.options.Helper = h
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/gui/api/workbuddy-credits", nil))
	if w.Code != 401 || h.calls != 0 {
		t.Fatal("unauthenticated balance disclosure")
	}
	token, err := s.auth.login("127.0.0.1", "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/gui/api/workbuddy-credits", nil)
	r.Header.Set("Cookie", "cpa_session="+token)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || h.calls != 1 || !strings.Contains(w.Body.String(), "2195") {
		t.Fatalf("credit route missing: %d %s", w.Code, w.Body)
	}
}

func TestWorkBuddyPanelRemainsIndependentFromOAuthAndReset(t *testing.T) {
	for _, expected := range []string{"id=\"workbuddyCreditsPanel\"", "loadWorkBuddyCredits(workbuddyPanel)", "api('/workbuddy-credits')", "上游剩余积分", "不按 0 积分处理", "刷新显示只重新读取插件缓存"} {
		if !strings.Contains(string(index), expected) {
			t.Fatalf("missing WorkBuddy UI behavior: %s", expected)
		}
	}
}
