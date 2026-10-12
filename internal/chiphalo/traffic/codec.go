// Package traffic implements transport encoding without changing API semantics.
package traffic

import (
	"compress/gzip"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const MaxBodyBytes int64 = 64 << 20

func Encoder(w io.Writer, coding string) (interface {
	io.WriteCloser
	Flush() error
}, error) {
	switch coding {
	case "gzip":
		return gzip.NewWriterLevel(w, gzip.BestSpeed)
	case "br":
		return brotli.NewWriterLevel(w, 2), nil
	case "zstd":
		return zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<20))
	default:
		return nil, fmt.Errorf("unsupported content encoding")
	}
}

type decoder struct {
	io.Reader
	close func() error
}

func (d decoder) Close() error { return d.close() }

func Decoder(r io.Reader, coding string) (io.ReadCloser, error) {
	switch strings.ToLower(strings.TrimSpace(coding)) {
	case "gzip":
		return gzip.NewReader(r)
	case "br":
		return decoder{brotli.NewReader(r), func() error { return nil }}, nil
	case "zstd":
		z, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(MaxBodyBytes)), zstd.WithDecoderMaxWindow(8<<20))
		if err != nil {
			return nil, err
		}
		return decoder{z, func() error { z.Close(); return nil }}, nil
	default:
		return nil, fmt.Errorf("unsupported content encoding")
	}
}

// Negotiate honors explicit exclusions before wildcard preferences.
func Negotiate(header string) string {
	quality := qualities(header)
	best, bestQ := "", 0.0
	for _, name := range []string{"zstd", "br", "gzip"} {
		q, ok := quality[name]
		if !ok {
			q = quality["*"]
		}
		if q > bestQ {
			best, bestQ = name, q
		}
	}
	if identity, explicit := quality["identity"]; explicit && identity > bestQ {
		return ""
	}
	return best
}

func identityAllowed(header string) bool {
	q := qualities(header)
	if quality, ok := q["identity"]; ok {
		return quality > 0
	}
	if quality, ok := q["*"]; ok && quality == 0 {
		return false
	}
	return true
}

func qualities(header string) map[string]float64 {
	quality := map[string]float64{}
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(item, ";")
		name, q := strings.ToLower(strings.TrimSpace(parts[0])), 1.0
		for _, p := range parts[1:] {
			kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
			if len(kv) == 2 && strings.EqualFold(kv[0], "q") {
				v, err := strconv.ParseFloat(kv[1], 64)
				if err != nil || v < 0 || v > 1 || v != v {
					q = 0
				} else {
					q = v
				}
			}
		}
		quality[name] = q
	}
	return quality
}
