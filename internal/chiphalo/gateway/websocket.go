package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/compat"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/opencode"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func websocketAdmission(c *gin.Context, options AdmissionOptions) bool {
	if c.Request.Method != http.MethodGet || (c.Request.URL.Path != "/v1/responses" && c.Request.URL.Path != "/backend-api/codex/responses") {
		return false
	}
	if !strings.EqualFold(c.Request.Header.Get("Upgrade"), "websocket") {
		abort(c, 400, "websocket upgrade required")
		return true
	}
	if origin := c.Request.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host != c.Request.Host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			abort(c, 403, "websocket origin rejected")
			return true
		}
	}
	principal, ip, path := c.GetString("userApiKey"), c.ClientIP(), c.Request.URL.Path
	keyID := func() string {
		if options.KeyID == nil {
			return ""
		}
		return options.KeyID(principal)
	}
	if keyID() == "" {
		abort(c, 403, "authenticated key has no CPA audit identity")
		return true
	}
	select {
	case options.websocketSlots <- struct{}{}:
		defer func() { <-options.websocketSlots }()
	default:
		abort(c, 429, "CPA websocket connection limit reached")
		return true
	}
	lastModel, lastAuth := "", ""
	hook := &handlers.WebsocketAdmission{MaxBytes: MaxBodyBytes}
	hook.CheckFrame = func(body []byte) error {
		if keyID() == "" {
			return &handlers.AdmissionError{Status: 403, Message: "CPA key no longer active"}
		}
		if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() || strictContinuationFields(body) != nil {
			return &handlers.AdmissionError{Status: 400, Message: "ambiguous websocket request"}
		}
		if gjson.GetBytes(body, "stream_id").Exists() {
			return &handlers.AdmissionError{Status: 400, Message: "multiplexed websocket lanes are not supported by this CPA profile"}
		}
		switch gjson.GetBytes(body, "type").String() {
		case "response.create", "response.interrupt":
			return nil
		default:
			return &handlers.AdmissionError{Status: 400, Message: "unsupported CPA websocket event"}
		}
	}
	hook.AdmitTurn = func(parent context.Context, body []byte) (context.Context, []byte, string, error) {
		deny := func(status int, message string) (context.Context, []byte, string, error) {
			return nil, nil, "", &handlers.AdmissionError{Status: status, Message: message}
		}
		if err := hook.CheckFrame(body); err != nil {
			return nil, nil, "", err
		}
		model := lastModel
		if raw := gjson.GetBytes(body, "model"); raw.Exists() {
			var err error
			model, err = requestModel(body)
			if err != nil {
				return deny(400, "invalid websocket model")
			}
		}
		if model == "" {
			return deny(400, "first websocket turn requires model")
		}
		id := keyID()
		if options.Authorize != nil && options.Authorize(id, model) != nil {
			return deny(403, "model is not permitted for this CPA key")
		}
		routed, authID := model, ""
		if options.Resolve != nil {
			var err error
			routed, authID, err = options.Resolve(model, path)
			if err != nil {
				return deny(400, "CPA websocket model route unavailable")
			}
		}
		previous := gjson.GetBytes(body, "previous_response_id")
		if previous.Type == gjson.String && previous.String() != "" && (model != lastModel || authID != lastAuth) {
			return deny(409, "continuation cannot change CPA model or account; replay full context")
		}
		var err error
		body, err = sjson.SetBytes(body, "model", routed)
		if err != nil {
			return deny(400, "cannot resolve websocket model")
		}
		body, err = compat.NormalizeOpenCodeTools(body)
		if err != nil {
			return deny(400, "invalid websocket tools")
		}
		if options.ChargeTurn != nil && !options.ChargeTurn(id) {
			return deny(429, "CPA websocket turn rate limit reached")
		}
		meta := requestMetadata{Model: model, KeyID: id, IP: ip, Path: path, Started: time.Now()}
		decorateSnapshot := options.Decorate
		if options.SnapshotDecorator != nil {
			decorateSnapshot = options.SnapshotDecorator()
		}
		decorate := func(ctx context.Context) context.Context {
			ctx = context.WithValue(ctx, metadataKey{}, meta)
			ctx = usage.WithRawUsageCapture(ctx)
			ctx = handlers.WithPinnedAuthID(ctx, authID)
			if decorateSnapshot != nil {
				ctx = decorateSnapshot(ctx, authID)
			}
			if strings.HasPrefix(model, "opencode/") {
				ctx = opencode.WithRecovery(ctx)
			}
			return ctx
		}
		ctx := handlers.WithExecutionContextDecorator(decorate(parent), decorate)
		lastModel, lastAuth = model, authID
		return ctx, body, authID, nil
	}
	c.Request = c.Request.WithContext(handlers.WithWebsocketAdmission(c.Request.Context(), hook))
	c.Next()
	return true
}
