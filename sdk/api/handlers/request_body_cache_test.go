package handlers

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type unexpectedBodyRead struct{}

func (unexpectedBodyRead) Read([]byte) (int, error) {
	return 0, errors.New("cached body was read twice")
}
func (unexpectedBodyRead) Close() error { return nil }

func TestReadRequestBodyReusesAdmittedBodyAndHonorsLimit(t *testing.T) {
	for _, limit := range []int64{0, 2} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Request.Body = unexpectedBodyRead{}
		body := []byte(`{"model":"fixture"}`)
		c.Set(gin.BodyBytesKey, body)
		c.Set(MaxRequestBodyBytesContextKey, limit)
		got, err := ReadRequestBody(c)
		if limit > 0 {
			var tooLarge *http.MaxBytesError
			if !errors.As(err, &tooLarge) {
				t.Fatal("cached bytes bypassed body limit")
			}
		} else if err != nil || len(got) != len(body) || &got[0] != &body[0] {
			t.Fatal("admitted bytes were copied or re-read")
		}
	}
}

func TestReadRequestBodySizeHintPreservesShortAndLongStreams(t *testing.T) {
	for _, hint := range []int64{1, 8, 20} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader("12345678"))
		c.Request.ContentLength = hint
		raw, err := ReadRequestBody(c)
		if err != nil || string(raw) != "12345678" {
			t.Fatalf("size hint changed bytes: %d %q %v", hint, raw, err)
		}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", io.NopCloser(strings.NewReader("")))
	_, err := ReadRequestBody(c)
	if err != nil {
		t.Fatal(err)
	}
}
