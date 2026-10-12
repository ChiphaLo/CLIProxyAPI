package traffic

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// UpstreamRequestCompression requires a reviewed, credential-specific capability.
// Never infer support from response Accept-Encoding, and never retry a failed
// compressed POST as identity: the upstream may already have executed it.
func UpstreamRequestCompression(base http.RoundTripper, coding string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if coding == "" || coding == "identity" {
		return base
	}
	return requestCompression{base, coding}
}

type requestCompression struct {
	base   http.RoundTripper
	coding string
}

func (t requestCompression) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || r.Body == nil || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || r.Header.Get("Content-Encoding") != "" || r.ContentLength >= 0 && r.ContentLength < 1024 {
		return t.base.RoundTrip(r)
	}
	var b bytes.Buffer
	defer r.Body.Close()
	z, err := Encoder(&b, t.coding)
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(z, io.LimitReader(r.Body, MaxBodyBytes+1))
	errClose := z.Close()
	if err != nil {
		return nil, err
	}
	if errClose != nil {
		return nil, errClose
	}
	if n > MaxBodyBytes {
		return nil, fmt.Errorf("upstream request exceeds 64 MiB")
	}
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(bytes.NewReader(b.Bytes()))
	clone.ContentLength = int64(b.Len())
	clone.Header.Del("Content-Length")
	clone.Header.Set("Content-Encoding", t.coding)
	clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b.Bytes())), nil }
	return t.base.RoundTrip(clone)
}

func (t requestCompression) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
