package traffic

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func compressed(t *testing.T, p []byte, coding string) []byte {
	t.Helper()
	var b bytes.Buffer
	z, err := Encoder(&b, coding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = z.Write(p); err != nil {
		t.Fatal(err)
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func decoded(t *testing.T, p []byte, coding string) []byte {
	t.Helper()
	d, err := Decoder(bytes.NewReader(p), coding)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	v, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCompressionNegotiation(t *testing.T) {
	for _, tc := range []struct{ header, want string }{
		{"", ""}, {"gzip", "gzip"}, {"br, gzip;q=0.5", "br"}, {"zstd,br,gzip", "zstd"}, {"*;q=1,zstd;q=0,br;q=0", "gzip"}, {"gzip;q=0", ""}, {"gzip;q=bogus", ""}, {"gzip;q=NaN", ""}, {"gzip;q=.5,identity;q=1", ""}, {"GZIP;q=1", "gzip"},
	} {
		t.Run(tc.header, func(t *testing.T) {
			if got := Negotiate(tc.header); got != tc.want {
				t.Fatalf("%q != %q", got, tc.want)
			}
		})
	}
}

func TestEncodingRefusalAndSensitiveResponses(t *testing.T) {
	for _, tc := range []struct {
		accept, control, cookie string
		status                  int
		encoded                 bool
	}{
		{"gzip;q=0,br;q=0,zstd;q=0,identity;q=0", "", "", 406, false}, {"*;q=0", "", "", 406, false},
		{"gzip", "no-transform", "", 200, false}, {"gzip", "", "session=secret", 200, false}, {"gzip", "", "", 200, true},
	} {
		t.Run(fmt.Sprint(tc.accept, tc.control, tc.cookie), func(t *testing.T) {
			r := gin.New()
			r.Use(HTTP())
			r.GET("/v1/models", func(c *gin.Context) {
				c.Header("Cache-Control", tc.control)
				if tc.cookie != "" {
					c.Header("Set-Cookie", tc.cookie)
				}
				c.JSON(200, gin.H{"data": []any{}})
			})
			req := httptest.NewRequest("GET", "/v1/models", nil)
			req.Header.Set("Accept-Encoding", tc.accept)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.status || (w.Header().Get("Content-Encoding") != "") != tc.encoded {
				t.Fatal("encoding policy violated")
			}
		})
	}
}

func TestUpstreamResponseCompressionStreams(t *testing.T) {
	for _, coding := range []string{"gzip", "br", "zstd"} {
		t.Run(coding, func(t *testing.T) {
			finish := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != coding {
					t.Error("response capability ignored")
				}
				w.Header().Set("Content-Encoding", coding)
				w.Header().Set("Content-Type", "text/event-stream")
				z, _ := Encoder(w, coding)
				defer z.Close()
				_, _ = io.WriteString(z, "data: first\n\n")
				_ = z.Flush()
				w.(http.Flusher).Flush()
				<-finish
				_, _ = io.WriteString(z, "data: last\n\n")
			}))
			defer server.Close()
			defer close(finish)
			base := &http.Transport{DisableCompression: true}
			defer base.CloseIdleConnections()
			client := &http.Client{Transport: UpstreamResponseCompression(base, coding)}
			resp, err := client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.Header.Get("Content-Encoding") != "" || !resp.Uncompressed {
				t.Fatal("encoded headers reached parser")
			}
			b := make([]byte, len("data: first\n\n"))
			if _, err := io.ReadFull(resp.Body, b); err != nil || string(b) != "data: first\n\n" {
				t.Fatal("upstream decompressor buffered the stream")
			}
		})
	}
}

func TestHTTPCompressionWireRoundTrip(t *testing.T) {
	payload := []byte(`{"input":"` + strings.Repeat("transport-context-", 20000) + `"}`)
	for _, coding := range []string{"gzip", "br", "zstd"} {
		t.Run(coding, func(t *testing.T) {
			router := gin.New()
			router.Use(HTTP())
			router.POST("/v1/responses", func(c *gin.Context) {
				b, err := io.ReadAll(c.Request.Body)
				if err != nil || !bytes.Equal(b, payload) || c.Request.Header.Get("Content-Encoding") != "" {
					t.Error("request was not transparently decoded")
				}
				c.Data(200, "application/json", b)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			wire := compressed(t, payload, coding)
			r, _ := http.NewRequest("POST", server.URL+"/v1/responses", bytes.NewReader(wire))
			r.Header.Set("Content-Encoding", coding)
			r.Header.Set("Accept-Encoding", coding)
			client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
			defer client.CloseIdleConnections()
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != coding || !bytes.Equal(decoded(t, b, coding), payload) {
				t.Fatal("compressed response changed payload")
			}
			if len(wire) >= len(payload) || len(b) >= len(payload) {
				t.Fatal("no wire savings")
			}
			t.Logf("codec=%s identity=%d request_wire=%d response_wire=%d", coding, len(payload), len(wire), len(b))
		})
	}
}

func TestSSECompressionFlushBeforeCompletion(t *testing.T) {
	for _, coding := range []string{"gzip", "br", "zstd"} {
		t.Run(coding, func(t *testing.T) {
			finish := make(chan struct{})
			router := gin.New()
			router.Use(HTTP())
			router.POST("/v1/responses", func(c *gin.Context) {
				c.Header("Content-Type", "text/event-stream")
				_, _ = io.WriteString(c.Writer, "data: first\n\n")
				c.Writer.Flush()
				<-finish
				_, _ = io.WriteString(c.Writer, "data: last\n\n")
				c.Writer.Flush()
			})
			server := httptest.NewServer(router)
			defer server.CloseClientConnections()
			defer server.Close()
			defer close(finish)
			r, _ := http.NewRequest("POST", server.URL+"/v1/responses", nil)
			r.Header.Set("Accept-Encoding", coding)
			client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
			defer client.CloseIdleConnections()
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			d, err := Decoder(resp.Body, coding)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			first := make([]byte, len("data: first\n\n"))
			if _, err = io.ReadFull(d, first); err != nil || string(first) != "data: first\n\n" {
				t.Fatalf("first event was not readable before completion: %q %v", first, err)
			}
			// Closing the body cancels the client while the server is still waiting.
		})
	}
}

func TestCompressionRejectsBombAndInvalidEncoding(t *testing.T) {
	router := gin.New()
	router.Use(HTTP())
	router.POST("/v1/responses", func(c *gin.Context) { c.Status(200) })
	var bomb bytes.Buffer
	z := gzip.NewWriter(&bomb)
	_, _ = io.CopyN(z, zeroReader{}, MaxBodyBytes+1)
	_ = z.Close()
	for _, tc := range []struct {
		coding string
		body   []byte
		want   int
	}{{"deflate", []byte("x"), 415}, {"gzip", []byte("broken"), 400}, {"gzip,br", []byte("x"), 415}, {"gzip", bomb.Bytes(), 413}} {
		t.Run(fmt.Sprint(tc.coding, tc.want), func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(tc.body))
			r.Header.Set("Content-Encoding", tc.coding)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("%d != %d", w.Code, tc.want)
			}
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func TestUpstreamCompressionOptInAndNoRetry(t *testing.T) {
	payload := []byte(`{"input":"` + strings.Repeat("upstream-prompt-", 3000) + `"}`)
	for _, coding := range []string{"", "gzip", "br", "zstd"} {
		t.Run(coding, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				b, _ := io.ReadAll(r.Body)
				if coding != "" {
					if r.Header.Get("Content-Encoding") != coding {
						t.Error("capability ignored")
					}
					t.Logf("upstream codec=%s identity=%d wire=%d", coding, len(payload), len(b))
					b = decoded(t, b, coding)
				} else if r.Header.Get("Content-Encoding") != "" {
					t.Error("unapproved compression")
				}
				if !bytes.Equal(b, payload) {
					t.Error("payload semantics changed")
				}
				w.WriteHeader(415)
			}))
			defer server.Close()
			base := &http.Transport{}
			defer base.CloseIdleConnections()
			client := &http.Client{Transport: UpstreamRequestCompression(base, coding)}
			r, _ := http.NewRequest("POST", server.URL, bytes.NewReader(payload))
			r.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 415 || calls != 1 {
				t.Fatal("compressed POST was retried")
			}
		})
	}
}
