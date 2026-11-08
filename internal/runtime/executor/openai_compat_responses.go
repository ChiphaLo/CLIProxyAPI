package executor

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/compat"
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

// Only the selected credential's explicit capability selects this endpoint.
// Never retry a possibly executed request using another protocol.
func usesNativeResponses(auth *coreauth.Auth) bool {
	return auth != nil && auth.Attributes["upstream_protocol"] == "responses"
}

type nativeResponsesRequest struct {
	bridge         *opencode.MuseBridge
	body, original []byte
	model          string
	format         sdktranslator.Format
}

func (e *OpenAICompatExecutor) prepareNativeResponses(ctx context.Context, req coreexecutor.Request, opts coreexecutor.Options, stream bool) (*nativeResponsesRequest, error) {
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
	body, err = compat.NormalizeResponsesTools(body)
	if err != nil {
		return nil, statusErr{code: 400, msg: err.Error()}
	}
	body = helps.SetStringIfDifferent(body, "model", model)
	body = helps.SetBoolIfDifferent(body, "stream", stream)
	// A stateless proxy does not opt users into provider response storage. An
	// explicitly configured or requested value is still owned by the caller.
	if !gjson.GetBytes(body, "store").Exists() {
		body = helps.SetBoolIfDifferent(body, "store", false)
	}
	body, _ = sjson.DeleteBytes(body, "stream_options")
	body, bridge, err := opencode.PrepareNativeResponses(body, nil, "")
	if err != nil {
		return nil, statusErr{code: 400, msg: err.Error()}
	}
	// Payload rules are the final semantic barrier, including model, tools,
	// store and reasoning. Nothing below this call mutates the business payload.
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, model, to.String(), from.String(), "", body, originalTranslated, helps.PayloadRequestedModel(opts, req.Model), helps.PayloadRequestPath(opts), opts.Headers)
	if err := bridge.BindFinal(body); err != nil {
		return nil, statusErr{code: 400, msg: err.Error()}
	}
	return &nativeResponsesRequest{bridge: bridge, body: body, original: bytes.Clone(original), model: model, format: coreexecutor.ResponseFormatOrSource(opts)}, nil
}

func (e *OpenAICompatExecutor) requestNativeResponses(ctx context.Context, auth *coreauth.Auth, opts coreexecutor.Options, p *nativeResponsesRequest, reporter *helps.UsageReporter) (*http.Response, error) {
	base, key := e.resolveCredentials(auth)
	if base == "" {
		return nil, statusErr{code: 401, msg: "Responses provider base URL is missing"}
	}
	endpoint := strings.TrimRight(base, "/") + "/responses"
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(p.body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	// Let stream control response framing. Some compatible providers route mixed
	// Accept headers incorrectly; do not invent a client identity or MIME profile.
	util.ApplyCustomHeadersFromAttrs(r, auth.Attributes, opts.Headers)
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{URL: endpoint, Method: http.MethodPost, Headers: r.Header.Clone(), Body: p.body, Provider: e.Identifier(), AuthID: auth.ID, AuthLabel: auth.Label})
	client := reporter.TrackHTTPClientRoundTripOnly(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	response, err := client.Do(r)
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

func (e *OpenAICompatExecutor) executeNativeResponses(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (result coreexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	p, err := e.prepareNativeResponses(ctx, req, opts, false)
	if err != nil {
		return result, err
	}
	r := helps.NewExecutorUsageReporter(ctx, e, p.model, auth)
	defer r.TrackFailure(ctx, &err)
	r.SetTranslatedReasoningEffort(p.body, sdktranslator.FormatOpenAIResponse.String())
	response, err := e.requestNativeResponses(ctx, auth, opts, p, r)
	if err != nil {
		return result, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
	if err != nil || len(data) > 64<<20 {
		return result, statusErr{code: 502, msg: "Responses body disconnected or exceeded 64 MiB"}
	}
	status := gjson.GetBytes(data, "status").String()
	if !gjson.ValidBytes(data) || gjson.GetBytes(data, "object").String() != "response" || !gjson.GetBytes(data, "output").IsArray() || (status != "completed" && status != "incomplete") {
		return result, statusErr{code: 502, msg: "Upstream did not return a successful Responses object"}
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	r.ObserveResponseModel(data)
	helps.ObserveResponsesTokenEvent(r, data)
	var param any
	out, err := p.bridge.Response(data)
	if err != nil {
		return result, statusErr{code: 502, msg: helps.NativeResponsesContractMessage(err, data)}
	}
	if p.format != sdktranslator.FormatOpenAIResponse {
		out = sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAIResponse, p.format, req.Model, p.original, p.body, out, &param)
	}
	if len(out) == 0 {
		return result, statusErr{code: 502, msg: "Native Responses translation failed"}
	}
	r.Publish(ctx, helps.ParseOpenAIUsage(data))
	return coreexecutor.Response{Payload: out, Headers: response.Header.Clone()}, nil
}

func (e *OpenAICompatExecutor) executeNativeResponsesStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (_ *coreexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	p, err := e.prepareNativeResponses(ctx, req, opts, true)
	if err != nil {
		return nil, err
	}
	r := helps.NewExecutorUsageReporter(ctx, e, p.model, auth)
	defer r.TrackFailure(ctx, &err)
	r.SetTranslatedReasoningEffort(p.body, sdktranslator.FormatOpenAIResponse.String())
	response, err := e.requestNativeResponses(ctx, auth, opts, p, r)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		_ = response.Body.Close()
		return nil, statusErr{code: 502, msg: "Native Responses stream is not SSE"}
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
			failure := statusErr{code: 502, msg: message}
			r.PublishFailure(ctx, failure)
			emit(coreexecutor.StreamChunk{Err: failure})
		}
		process := func() bool {
			if len(frame) == 0 {
				return true
			}
			data := bytes.Clone(frame)
			frame = nil
			if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
				if !terminal {
					fail("Responses stream ended before completion")
					return false
				}
				return true
			}
			if !gjson.ValidBytes(data) {
				fail("Upstream returned invalid Responses SSE JSON")
				return false
			}
			kind := gjson.GetBytes(data, "type").String()
			if kind == "" || terminal {
				fail("Upstream returned invalid Responses event ordering")
				return false
			}
			r.ObserveResponseModel(data)
			helps.ObserveResponsesTokenEvent(r, data)
			if kind == "error" || kind == "response.failed" {
				status, body, delay := helps.ResponsesStreamFailure(data)
				failure := newOpenAICompatStatusError(status, response.Header, body)
				if failure.retryAfter == nil {
					failure.retryAfter = delay
				}
				// Usage and diagnostics must not retain arbitrary upstream text.
				r.PublishFailure(ctx, statusErr{code: status, msg: "Upstream Responses stream reported failure"})
				emit(coreexecutor.StreamChunk{Err: failure})
				return false
			}
			events, errEvent := p.bridge.Event(data)
			if errEvent != nil {
				fail(helps.NativeResponsesContractMessage(errEvent, data))
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
				if p.format == sdktranslator.FormatOpenAIResponse {
					chunks = [][]byte{append(append([]byte("data: "), event...), '\n', '\n')}
				} else {
					chunks = sdktranslator.TranslateStream(ctx, sdktranslator.FormatOpenAIResponse, p.format, req.Model, p.original, p.body, append([]byte("data: "), event...), &param)
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
					fail("Responses SSE event exceeds 4 MiB")
					return
				}
				if len(frame) > 0 {
					frame = append(frame, '\n')
				}
				frame = append(frame, part...)
			}
		}
		if scanner.Err() != nil {
			fail("Responses stream disconnected or exceeded its event limit")
			return
		}
		if !process() {
			return
		}
		if !terminal {
			fail("Responses stream disconnected before completion")
		}
	}()
	return &coreexecutor.StreamResult{Headers: response.Header.Clone(), Chunks: out}, nil
}
