package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// MaxRequestBodyBytesContextKey stores the configured request body size limit (in bytes) on the Gin context.
const MaxRequestBodyBytesContextKey = "maxRequestBodyBytes"

// ReadRequestBody reads the incoming request body and decodes supported
// Content-Encoding values before handlers inspect JSON fields.
func ReadRequestBody(c *gin.Context) (body []byte, bodyErr error) {
	defer func() {
		if c == nil || c.Request == nil {
			return
		}
		if bodyErr == nil && strings.HasSuffix(c.Request.URL.Path, "/responses") {
			key := c.GetString("userApiKey")
			if id := coresession.VerifyBridge(c.Request.Header, key, body, time.Now()); id != "" {
				c.Set("cpa:verified-derived-session", id)
			}
		} else {
			c.Request.Header.Del(coresession.BridgeHeader)
		}
	}()
	maxBytes := int64(0)
	if c != nil {
		maxBytes = c.GetInt64(MaxRequestBodyBytesContextKey)
	}
	var raw []byte
	var err error
	if cached, exists := c.Get(gin.BodyBytesKey); exists {
		raw, _ = cached.([]byte)
	}
	if raw == nil {
		if c.Request != nil && c.Request.Body != nil && c.Request.ContentLength > 0 && c.Request.ContentLength <= 64<<20 {
			// Reserve once for ordinary HTTP bodies instead of repeatedly growing
			// io.ReadAll's buffer across a multi-megabyte inline attachment.
			var buffer bytes.Buffer
			buffer.Grow(int(c.Request.ContentLength) + bytes.MinRead)
			_, err = buffer.ReadFrom(c.Request.Body)
			raw = buffer.Bytes()
		} else {
			raw, err = c.GetRawData()
		}
	}
	if err != nil {
		return nil, err
	}

	encoding := ""
	if c != nil && c.Request != nil {
		encoding = strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
	}
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		if maxBytes > 0 && int64(len(raw)) > maxBytes {
			return nil, &http.MaxBytesError{Limit: maxBytes}
		}
		return raw, nil
	}

	decoded, err := decodeRequestBody(raw, encoding, maxBytes)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return nil, err
		}
		if json.Valid(raw) {
			return raw, nil
		}
		return nil, err
	}
	return decoded, nil
}

func decodeRequestBody(raw []byte, encoding string, maxBytes int64) ([]byte, error) {
	parts := strings.Split(encoding, ",")
	body := raw
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, err := decodeZstdRequestBody(body, maxBytes)
			if err != nil {
				return nil, err
			}
			body = decoded
		default:
			return nil, fmt.Errorf("unsupported request content encoding: %s", enc)
		}
	}
	return body, nil
}

func decodeZstdRequestBody(raw []byte, maxBytes int64) ([]byte, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", err)
	}
	defer decoder.Close()

	var reader io.Reader = decoder
	if maxBytes > 0 && maxBytes < math.MaxInt64 {
		reader = io.LimitReader(decoder, maxBytes+1)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to decode zstd request body: %w", err)
	}
	if maxBytes > 0 && int64(len(decoded)) > maxBytes {
		return nil, &http.MaxBytesError{Limit: maxBytes}
	}
	return decoded, nil
}
