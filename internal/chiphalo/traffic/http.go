package traffic

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// HTTP belongs after authentication and before admission. Management/UI routes
// must not use it: compressing secret-bearing HTML can create length oracles.
func HTTP() gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.EqualFold(c.Request.Header.Get("Upgrade"), "websocket") {
			c.Next()
			return
		}
		coding := strings.ToLower(strings.TrimSpace(strings.Join(c.Request.Header.Values("Content-Encoding"), ",")))
		if coding != "" && coding != "identity" {
			if coding != "gzip" && coding != "br" && coding != "zstd" {
				fail(c, 415, "unsupported Content-Encoding")
				return
			}
			original := c.Request.Body
			defer original.Close()
			d, err := Decoder(http.MaxBytesReader(c.Writer, original, MaxBodyBytes), coding)
			if err != nil {
				fail(c, 400, "invalid compressed request")
				return
			}
			body, err := io.ReadAll(io.LimitReader(d, MaxBodyBytes+1))
			_ = d.Close()
			var max *http.MaxBytesError
			if int64(len(body)) > MaxBodyBytes || errors.As(err, &max) {
				fail(c, 413, "request body exceeds 64 MiB")
				return
			}
			if err != nil {
				fail(c, 400, "invalid compressed request")
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Request.ContentLength = int64(len(body))
			c.Request.Header.Del("Content-Encoding")
			c.Request.Header.Del("Content-Length")
		}
		c.Header("Vary", appendVary(c.Writer.Header().Get("Vary"), "Accept-Encoding"))
		coding = Negotiate(c.Request.Header.Get("Accept-Encoding"))
		if coding == "" {
			if !identityAllowed(c.Request.Header.Get("Accept-Encoding")) {
				fail(c, 406, "no acceptable response content encoding")
				return
			}
			c.Next()
			return
		}
		w := &encodedWriter{ResponseWriter: c.Writer, coding: coding}
		c.Writer = w
		defer func() {
			if w.encoder != nil {
				_ = w.encoder.Close()
			}
		}()
		c.Next()
	}
}

func fail(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "message": message}})
}

func appendVary(existing, token string) string {
	for _, v := range strings.Split(existing, ",") {
		if strings.EqualFold(strings.TrimSpace(v), token) || strings.TrimSpace(v) == "*" {
			return existing
		}
	}
	if existing == "" {
		return token
	}
	return existing + ", " + token
}

type encodedWriter struct {
	gin.ResponseWriter
	coding  string
	encoder interface {
		io.WriteCloser
		Flush() error
	}
	started bool
}

func (w *encodedWriter) start() {
	if w.started {
		return
	}
	w.started = true
	h := w.Header()
	ct := strings.ToLower(strings.SplitN(h.Get("Content-Type"), ";", 2)[0])
	if w.Status() != 200 || (ct != "application/json" && ct != "text/event-stream") || h.Get("Content-Encoding") != "" || h.Get("Set-Cookie") != "" || strings.Contains(strings.ToLower(h.Get("Cache-Control")), "no-transform") {
		return
	}
	w.encoder, _ = Encoder(w.ResponseWriter, w.coding)
	if w.encoder != nil {
		h.Set("Content-Encoding", w.coding)
		h.Del("Content-Length")
		h.Del("ETag")
	}
}

func (w *encodedWriter) Write(p []byte) (int, error) {
	w.start()
	if w.encoder != nil {
		return w.encoder.Write(p)
	}
	return w.ResponseWriter.Write(p)
}
func (w *encodedWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *encodedWriter) WriteHeaderNow()                   { w.start(); w.ResponseWriter.WriteHeaderNow() }
func (w *encodedWriter) Flush() {
	w.start()
	if w.encoder != nil {
		_ = w.encoder.Flush()
	}
	w.ResponseWriter.Flush()
}
