// Package gateway adapts the deployed CPA contracts to the upstream Go runtime.
// Protocol translation is owned by upstream; this package is admission and audit.
package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/compat"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/nativegemini"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/opencode"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const MaxBodyBytes int64 = 64 * 1024 * 1024

type requestMetadata struct {
	Model, KeyID, IP, Path string
	Started                time.Time
	AccountAuthID          string
	Account                AccountBinding
}
type metadataKey struct{}

// Admission must be installed with api.WithProxyMiddleware, after authentication.
// keyID maps an authenticated principal to a public key identifier, not a secret.
func Admission(keyID func(string) string) gin.HandlerFunc {
	return AdmissionWithOptions(AdmissionOptions{KeyID: keyID})
}

type AdmissionOptions struct {
	KeyID             func(string) string
	Authorize         func(keyID, model string) error
	Resolve           func(model, path string) (routedModel, authID string, err error)
	Decorate          func(context.Context, string) context.Context
	SnapshotDecorator func() func(context.Context, string) context.Context
	Continuations     *ConversationStore
	ChargeTurn        func(keyID string) bool
	websocketSlots    chan struct{}
}

func AdmissionWithOptions(options AdmissionOptions) gin.HandlerFunc {
	if options.Continuations == nil {
		options.Continuations = NewConversationStore()
	}
	if options.websocketSlots == nil {
		options.websocketSlots = make(chan struct{}, 8)
	}
	return func(c *gin.Context) {
		if websocketAdmission(c, options) {
			return
		}
		urlModel, nativeAction, native := nativegemini.Parse(c.Request.Method, c.Request.URL.Path)
		if c.Request.Method != http.MethodPost || (!native && !strings.HasSuffix(c.Request.URL.Path, "/responses") && !strings.HasSuffix(c.Request.URL.Path, "/chat/completions")) {
			c.Next()
			return
		}
		started := time.Now()
		if c.Request.ContentLength > MaxBodyBytes {
			abort(c, http.StatusRequestEntityTooLarge, "request body exceeds 64 MiB")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, MaxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				abort(c, http.StatusRequestEntityTooLarge, "request body exceeds 64 MiB")
			} else {
				abort(c, http.StatusBadRequest, "cannot read request body")
			}
			return
		}
		if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
			abort(c, http.StatusBadRequest, "request body must be a JSON object")
			return
		}
		model := urlModel
		if native {
			err = validateGeminiBody(body, urlModel, nativeAction)
		} else {
			model, err = requestModel(body)
		}
		if err != nil {
			abort(c, http.StatusBadRequest, "request requires one unambiguous string model")
			return
		}
		if strings.HasSuffix(c.Request.URL.Path, "/responses") {
			body, err = compat.NormalizeOpenCodeTools(body)
			if err != nil {
				// Errors contain schema metadata, never the request text or credentials.
				abort(c, http.StatusBadRequest, err.Error())
				return
			}
		}
		publicKeyID := ""
		if options.KeyID != nil {
			publicKeyID = options.KeyID(c.GetString("userApiKey"))
		}
		if publicKeyID == "" {
			abort(c, http.StatusForbidden, "authenticated key has no CPA audit identity")
			return
		}
		if options.Authorize != nil && options.Authorize(publicKeyID, model) != nil {
			abort(c, http.StatusForbidden, "model is not permitted for this CPA key")
			return
		}
		pinnedAuthID := ""
		if options.Resolve != nil {
			routed, authID, err := options.Resolve(model, c.Request.URL.Path)
			if err != nil {
				abort(c, http.StatusBadRequest, err.Error())
				return
			}
			pinnedAuthID = authID
			if routed != model {
				if native {
					abort(c, http.StatusBadRequest, "native Gemini model routing must preserve the URL resource")
					return
				}
				body, err = sjson.SetBytes(body, "model", routed)
				if err != nil {
					abort(c, http.StatusBadRequest, "cannot resolve CPA model route")
					return
				}
			}
		}
		metadata := requestMetadata{
			Model: model, KeyID: publicKeyID, IP: c.ClientIP(), Path: c.Request.URL.Path, Started: started,
		}
		registeredAuthID := pinnedAuthID
		var continuation *continuationWriter
		credentialHash := sha256.Sum256([]byte(c.Request.Header.Get("Authorization")))
		conversationID := conversationKey{Key: publicKeyID + ":" + hex.EncodeToString(credentialHash[:]), Model: model, Path: c.Request.URL.Path}
		if strings.HasSuffix(c.Request.URL.Path, "/responses") {
			body, pinnedAuthID, err = options.Continuations.prepare(body, conversationID, pinnedAuthID)
			if err != nil {
				abort(c, http.StatusConflict, err.Error())
				return
			}
			continuation = &continuationWriter{ResponseWriter: c.Writer}
			c.Writer = continuation
		}
		var selected atomic.Value
		selected.Store(pinnedAuthID)
		ctx := context.WithValue(c.Request.Context(), metadataKey{}, metadata)
		ctx = usage.WithRawUsageCapture(ctx)
		ctx = handlers.WithPinnedAuthID(ctx, pinnedAuthID)
		ctx = handlers.WithSelectedAuthIDCallback(ctx, func(id string) { selected.Store(id) })
		decorate := options.Decorate
		if options.SnapshotDecorator != nil {
			decorate = options.SnapshotDecorator()
		}
		if decorate != nil {
			ctx = decorate(ctx, registeredAuthID)
		}
		if strings.HasPrefix(model, "opencode/") {
			ctx = opencode.WithRecovery(ctx)
		}
		ctx = handlers.WithExecutionContextDecorator(ctx, func(parent context.Context) context.Context {
			decorated := usage.WithRawUsageCapture(context.WithValue(parent, metadataKey{}, metadata))
			decorated = handlers.WithPinnedAuthID(decorated, pinnedAuthID)
			decorated = handlers.WithSelectedAuthIDCallback(decorated, func(id string) { selected.Store(id) })
			if decorate != nil {
				decorated = decorate(decorated, registeredAuthID)
			}
			if strings.HasPrefix(model, "opencode/") {
				decorated = opencode.WithRecovery(decorated)
			}
			return decorated
		})
		c.Request = c.Request.WithContext(ctx)
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
		c.Next()
		if continuation != nil && c.Request.Context().Err() == nil {
			options.Continuations.save(conversationID, body, continuation.completed(), selected.Load().(string))
		}
	}
}

// Different JSON readers must not route/account for different occurrences of
// model. Reject duplicates, including escaped-equivalent top-level key names.
func requestModel(body []byte) (string, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	if _, err := d.Token(); err != nil {
		return "", err
	}
	model, found := "", false
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return "", err
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return "", err
		}
		if key == "model" {
			if found || json.Unmarshal(value, &model) != nil {
				return "", errors.New("ambiguous model")
			}
			found = true
		}
	}
	if !found || strings.TrimSpace(model) == "" || len(model) > 1024 || strings.IndexFunc(model, unicode.IsControl) >= 0 {
		return "", errors.New("missing or invalid model")
	}
	return model, nil
}

func abort(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "message": message}})
}
