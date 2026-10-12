package traffic

import (
	"fmt"
	"io"
	"net/http"
)

// UpstreamResponseCompression is independently opt-in. Request-body support
// and response encoding negotiation are different provider capabilities.
// Identity/default retains Go's automatic gzip behavior unchanged.
func UpstreamResponseCompression(base http.RoundTripper, coding string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if coding == "" || coding == "identity" {
		return base
	}
	return responseCompression{base, coding}
}

type responseCompression struct {
	base   http.RoundTripper
	coding string
}

func (t responseCompression) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t responseCompression) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if clone.Header.Get("Accept-Encoding") == "" {
		clone.Header.Set("Accept-Encoding", t.coding)
	}
	response, err := t.base.RoundTrip(clone)
	if err != nil || response == nil {
		return response, err
	}
	encoding := response.Header.Get("Content-Encoding")
	if encoding == "" || encoding == "identity" || response.Body == nil {
		return response, nil
	}
	decoded, err := Decoder(response.Body, encoding)
	if err != nil {
		_ = response.Body.Close()
		return nil, fmt.Errorf("invalid upstream response encoding")
	}
	response.Body = &decodedResponse{Reader: &boundedDecoded{reader: decoded, left: MaxBodyBytes}, decoded: decoded, original: response.Body}
	response.Header.Del("Content-Encoding")
	response.Header.Del("Content-Length")
	response.Header.Del("ETag")
	response.ContentLength = -1
	response.Uncompressed = true
	return response, nil
}

type decodedResponse struct {
	io.Reader
	decoded, original io.ReadCloser
}

func (r *decodedResponse) Close() error { _ = r.decoded.Close(); return r.original.Close() }

type boundedDecoded struct {
	reader io.Reader
	left   int64
}

func (r *boundedDecoded) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.left == 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("upstream decoded response exceeds 64 MiB")
		}
		return 0, err
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.reader.Read(p)
	r.left -= int64(n)
	return n, err
}
