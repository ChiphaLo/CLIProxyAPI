package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/opencode"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var museReferences = opencode.NewMuseReferences()

// Selected credential configuration, not a caller-supplied prefix, enables the
// native endpoint. Other providers and the upstream Meta executor are untouched.
func isOpenCodeMuse(auth *coreauth.Auth, req coreexecutor.Request) bool {
	return auth != nil && auth.Prefix == "opencode" && strings.HasPrefix(thinking.ParseSuffix(req.Model).ModelName, "muse-spark-")
}

type musePrepared struct {
	body, original []byte
	bridge         *opencode.MuseBridge
	session        string
	baseModel      string
	responseFormat sdktranslator.Format
}

func (e *OpenAICompatExecutor) prepareMuse(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, stream bool) (*musePrepared, error) {
	if opts.Alt != "" {
		return nil, statusErr{code: 400, msg: "Muse does not support this alternative endpoint"}
	}
	from, to := opts.SourceFormat, sdktranslator.FormatOpenAIResponse
	original := req.Payload
	if len(opts.OriginalRequest) > 0 {
		original = opts.OriginalRequest
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	originalTranslated, body, changed, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityAndUpdateIntent(ctx, opts.Headers, e.cfg, from, to, model, original, req.Payload, stream, helps.APIKeyModelIsCompat(req))
	if err != nil {
		return nil, err
	}
	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier(), changed)
	if err != nil {
		return nil, err
	}
	body = helps.SetStringIfDifferent(body, "model", model)
	body = helps.SetBoolIfDifferent(body, "stream", stream)
	for _, field := range []string{"stream_options", "generate", "client_metadata"} {
		body, _ = sjson.DeleteBytes(body, field)
	}
	session := ""
	for _, header := range []string{"x-opencode-session", "session_id", "x-codex-session-id", "x-claude-code-session-id"} {
		if value := opts.Headers.Get(header); value != "" {
			session = value
			break
		}
	}
	if session == "" {
		session = gjson.GetBytes(body, "prompt_cache_key").String()
	}
	if session == "" {
		for _, item := range gjson.GetBytes(body, "input").Array() {
			if item.Get("role").String() == "user" {
				session = item.Raw
				break
			}
		}
		if session == "" {
			session = util.SessionIDFromContext(ctx)
		}
	}
	if len(session) > 64<<20 {
		return nil, statusErr{code: 400, msg: "Muse session identity exceeds limit"}
	}
	// Do not expose a downstream key or an arbitrary supplied session header.
	_, credential := e.resolveCredentials(auth)
	sum := sha256.Sum256([]byte(auth.ID + "\x00" + credential + "\x00" + model + "\x00" + helps.APIKeyFromContext(ctx) + "\x00" + session))
	scope := hex.EncodeToString(sum[:])
	body, bridge, err := opencode.PrepareMuse(body, museReferences, scope)
	if err != nil {
		return nil, statusErr{code: 400, msg: err.Error()}
	}
	// The last semantic barrier. Everything below is read-only validation or
	// transport framing; filtered tools/settings cannot be restored by the bridge.
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, model, to.String(), from.String(), "", body, originalTranslated, helps.PayloadRequestedModel(opts, req.Model), helps.PayloadRequestPath(opts), opts.Headers)
	if err := bridge.BindFinal(body); err != nil {
		return nil, statusErr{code: 400, msg: err.Error()}
	}
	return &musePrepared{body: body, original: bytes.Clone(original), bridge: bridge, session: "cpa-go-" + scope, baseModel: model, responseFormat: coreexecutor.ResponseFormatOrSource(opts)}, nil
}

func (e *OpenAICompatExecutor) requestMuse(ctx context.Context, auth *coreauth.Auth, opts coreexecutor.Options, p *musePrepared, reporter *helps.UsageReporter) (*http.Response, error) {
	base, key := e.resolveCredentials(auth)
	if base == "" {
		return nil, statusErr{code: 401, msg: "Muse provider base URL is missing"}
	}
	endpoint := strings.TrimRight(base, "/") + "/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(p.body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "cpa-coding-gateway/1.0")
	req.Header.Set("Accept", "text/event-stream, application/json")
	util.ApplyCustomHeadersFromAttrs(req, auth.Attributes, opts.Headers)
	req.Header.Set("x-opencode-session", p.session)
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{URL: endpoint, Method: http.MethodPost, Headers: req.Header.Clone(), Body: p.body, Provider: e.Identifier(), AuthID: auth.ID, AuthLabel: auth.Label})
	client := reporter.TrackHTTPClientRoundTripOnly(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, response.StatusCode, response.Header.Clone())
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		_ = response.Body.Close()
		return nil, newOpenAICompatStatusError(response.StatusCode, response.Header, body)
	}
	return response, nil
}

func (e *OpenAICompatExecutor) executeMuse(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (result coreexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	p, err := e.prepareMuse(ctx, auth, req, opts, false)
	if err != nil {
		return result, err
	}
	r := helps.NewExecutorUsageReporter(ctx, e, p.baseModel, auth)
	defer r.TrackFailure(ctx, &err)
	r.SetTranslatedReasoningEffort(p.body, sdktranslator.FormatOpenAIResponse.String())
	response, err := e.requestMuse(ctx, auth, opts, p, r)
	if err != nil {
		return result, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
	if err != nil || len(data) > 64<<20 {
		return result, statusErr{code: 502, msg: "Muse response read failed or exceeded 64 MiB"}
	}
	if !gjson.ValidBytes(data) || gjson.GetBytes(data, "object").String() != "response" {
		return result, statusErr{code: 502, msg: "Muse did not return a Responses object"}
	}
	status := gjson.GetBytes(data, "status").String()
	if (status != "completed" && status != "incomplete") || !gjson.GetBytes(data, "output").IsArray() {
		return result, statusErr{code: 502, msg: "Muse returned a failed or invalid response"}
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	r.ObserveResponseModel(data)
	helps.ObserveResponsesTokenEvent(r, data)
	out, err := p.bridge.Response(data)
	if err != nil {
		return result, statusErr{code: 502, msg: opencode.MuseFormatError}
	}
	var param any
	out = sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAIResponse, p.responseFormat, req.Model, p.original, p.body, out, &param)
	if len(out) == 0 {
		return result, statusErr{code: 502, msg: "Muse response translation failed"}
	}
	r.Publish(ctx, helps.ParseOpenAIUsage(data))
	return coreexecutor.Response{Payload: out, Headers: response.Header.Clone()}, nil
}

func (e *OpenAICompatExecutor) executeMuseStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (_ *coreexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	p, err := e.prepareMuse(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	r := helps.NewExecutorUsageReporter(ctx, e, p.baseModel, auth)
	defer r.TrackFailure(ctx, &err)
	r.SetTranslatedReasoningEffort(p.body, sdktranslator.FormatOpenAIResponse.String())
	response, err := e.requestMuse(ctx, auth, opts, p, r)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		_ = response.Body.Close()
		return nil, statusErr{code: 502, msg: "Muse streaming response is not SSE"}
	}
	out := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() { _ = response.Body.Close() }()
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(nil, 4<<20)
		var frame []byte
		var param any
		terminal := false
		emit := func(chunk coreexecutor.StreamChunk) bool {
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}
		fail := func(message string) {
			err := statusErr{code: 502, msg: message}
			r.PublishFailure(ctx, err)
			emit(coreexecutor.StreamChunk{Err: err})
		}
		process := func() bool {
			if len(frame) == 0 {
				return true
			}
			data := bytes.Clone(frame)
			frame = nil
			if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
				if !terminal {
					fail("Muse stream ended before a terminal response")
					return false
				}
				return true
			}
			if !gjson.ValidBytes(data) {
				fail("Muse returned an invalid SSE event")
				return false
			}
			kind := gjson.GetBytes(data, "type").String()
			if terminal {
				fail("Muse emitted data after a terminal response")
				return false
			}
			r.ObserveResponseModel(data)
			helps.ObserveResponsesTokenEvent(r, data)
			if kind == "error" || kind == "response.failed" {
				fail("Muse upstream stream failed")
				return false
			}
			events, errEvent := p.bridge.Event(data)
			if errEvent != nil {
				fail(opencode.MuseFormatError)
				return false
			}
			if kind == "response.completed" || kind == "response.incomplete" {
				terminal = true
				if detail, ok := helps.ParseCodexUsage(data); ok {
					r.Publish(ctx, detail)
				} else {
					r.EnsurePublished(ctx)
				}
			}
			for _, event := range events {
				var chunks [][]byte
				if p.responseFormat == sdktranslator.FormatOpenAIResponse {
					chunks = [][]byte{append(append([]byte("data: "), event...), '\n', '\n')}
				} else {
					chunks = sdktranslator.TranslateStream(ctx, sdktranslator.FormatOpenAIResponse, p.responseFormat, req.Model, p.original, p.body, append([]byte("data: "), event...), &param)
				}
				for _, chunk := range chunks {
					if !emit(coreexecutor.StreamChunk{Payload: chunk}) {
						return false
					}
				}
			}
			return true
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				if !process() {
					return
				}
				continue
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				part := bytes.TrimPrefix(line, []byte("data:"))
				part = bytes.TrimPrefix(part, []byte(" "))
				if len(frame)+len(part)+1 > 4<<20 {
					fail("Muse SSE event exceeded 4 MiB")
					return
				}
				if len(frame) > 0 {
					frame = append(frame, '\n')
				}
				frame = append(frame, part...)
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			fail("Muse stream disconnected or exceeded its event limit")
			return
		}
		if !process() {
			return
		}
		if !terminal {
			fail("Muse stream disconnected before completion")
		}
	}()
	return &coreexecutor.StreamResult{Headers: response.Header.Clone(), Chunks: out}, nil
}
