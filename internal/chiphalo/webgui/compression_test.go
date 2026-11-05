package webgui

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestReadStatisticsCompressionPreservesJSONAndAuthentication(t *testing.T) {
	s, _ := fixtureServer(t)
	token, err := s.auth.login("127.0.0.1", "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, encoding string, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Accept-Encoding", encoding)
		if authenticated {
			r.AddCookie(&http.Cookie{Name: "cpa_session", Value: token})
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	plain, compressed := request("/api/usage", "", true), request("/api/usage", "gzip", true)
	if plain.Code != 200 || compressed.Code != 200 || compressed.Header().Get("Content-Encoding") != "gzip" || compressed.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("read response policy changed")
	}
	z, err := gzip.NewReader(compressed.Body)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	_ = z.Close()
	var a, b any
	if json.Unmarshal(plain.Body.Bytes(), &a) != nil || json.Unmarshal(raw, &b) != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("compression changed statistics")
	}
	if request("/api/usage", "gzip", false).Code != 401 {
		t.Fatal("authentication bypassed")
	}
	if request("/api/admin/csrf", "gzip", true).Header().Get("Content-Encoding") != "" {
		t.Fatal("CSRF secret compressed")
	}
	if request("/api/usage", "gzip;q=0", true).Header().Get("Content-Encoding") != "" {
		t.Fatal("gzip opt-out ignored")
	}
}
