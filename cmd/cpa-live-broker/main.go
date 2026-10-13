// cpa-live-broker is a bounded test-only credential broker. The Codex sandbox
// never receives production credentials, auth files, or filesystem access.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/traffic"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func run() error {
	source := flag.String("source-config", "", "Read-only private frozen source configuration")
	prefix := flag.String("prefix", "opencode", "Explicit source provider prefix")
	model := flag.String("model", "deepseek-v4.1-flash", "Exactly one allowed upstream model")
	listen := flag.String("listen", "127.0.0.1:18330", "Private test-network listener")
	ack := flag.Bool("acknowledge-live-upstream", false, "Authorize bounded real upstream token consumption")
	budget := flag.Int64("request-budget", 8, "Hard cap on real inference requests (1..12)")
	responsesBackend := flag.String("responses-backend", "", "Optional private test gateway for transparent downstream shape observation")
	upstreamCompression := flag.String("upstream-request-compression", "", "Explicit real-provider capability probe (empty, gzip, br, zstd); never retries as identity")
	nativeMuse := flag.Bool("native-muse", false, "Explicit opencode Muse Responses-only test; no Chat endpoint fallback")
	useSourceProxy := flag.Bool("source-proxy", false, "Explicitly use the frozen source egress proxy when reachable on the test host")
	flag.Parse()
	if !*ack || *source == "" || *budget < 1 || *budget > 12 {
		return fmt.Errorf("a private source, explicit live acknowledgment and request budget are required")
	}
	if *nativeMuse && (*prefix != "opencode" || !strings.HasPrefix(*model, "muse-spark-")) {
		return fmt.Errorf("native Muse probe requires explicit opencode Muse model")
	}
	upstreamPath := "/chat/completions"
	if *nativeMuse {
		upstreamPath = "/responses"
	}
	switch *upstreamCompression {
	case "", "gzip", "br", "zstd":
	default:
		return fmt.Errorf("invalid test upstream compression")
	}
	cfg, err := config.LoadConfig(*source)
	if err != nil {
		return fmt.Errorf("cannot load the private frozen source configuration")
	}
	var selected *config.OpenAICompatibility
	for i := range cfg.OpenAICompatibility {
		if cfg.OpenAICompatibility[i].Prefix == *prefix {
			if selected != nil {
				return fmt.Errorf("source provider prefix is ambiguous")
			}
			selected = &cfg.OpenAICompatibility[i]
		}
	}
	if selected == nil || len(selected.APIKeyEntries) == 0 {
		return fmt.Errorf("explicit source provider has no credential")
	}
	published := false
	for _, entry := range selected.Models {
		if entry.Name == *model {
			published = true
		}
	}
	if !published {
		return fmt.Errorf("test model is not published by this source provider")
	}
	endpoint, err := url.Parse(strings.TrimRight(selected.BaseURL, "/"))
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("source must have a plain HTTPS base URL")
	}
	key := selected.APIKeyEntries[0].APIKey
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return fmt.Errorf("invalid source credential")
	}
	var admitted atomic.Int64
	var observed atomic.Int64
	var downstream *httputil.ReverseProxy
	if *responsesBackend != "" {
		backend, err := url.Parse(*responsesBackend)
		if err != nil || backend.Scheme != "http" || !strings.HasPrefix(backend.Hostname(), "cpa-live-go-") || backend.Port() != "8317" || backend.User != nil || backend.RawQuery != "" || backend.Fragment != "" || backend.Path != "" {
			return fmt.Errorf("downstream observation requires a private isolated test gateway")
		}
		downstream = &httputil.ReverseProxy{
			Rewrite: func(req *httputil.ProxyRequest) {
				req.SetURL(backend)
				req.Out.Header = make(http.Header)
				req.Out.Header.Set("Content-Type", "application/json")
				req.Out.Header.Set("Authorization", "Bearer wire-client-test-only")
			},
			Transport: &http.Transport{Proxy: nil}, FlushInterval: -1,
			ModifyResponse: func(response *http.Response) error {
				response.Body = observeResponse(response.Body, response.StatusCode, response.Header.Get("Content-Type"))
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "private test gateway transport failed", http.StatusBadGateway)
			},
		}
	}
	var upstreamTransport http.RoundTripper = &http.Transport{Proxy: nil}
	if *useSourceProxy {
		upstreamTransport = helps.NewProxyAwareHTTPClient(context.Background(), cfg, &coreauth.Auth{ProxyURL: selected.APIKeyEntries[0].ProxyURL}, 0).Transport
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(req *httputil.ProxyRequest) {
			req.SetURL(endpoint)
			req.Out.URL.Path = strings.TrimRight(endpoint.Path, "/") + upstreamPath
			req.Out.URL.RawPath = ""
			req.Out.Header = make(http.Header)
			req.Out.Header.Set("Content-Type", "application/json")
			req.Out.Header.Set("Accept", "text/event-stream, application/json")
			req.Out.Header.Set("Authorization", "Bearer "+key)
			req.Out.Header.Set("User-Agent", "cpa-coding-gateway/1.0")
			if endpoint.Hostname() == "opencode.ai" {
				req.Out.Header.Set("x-opencode-session", "cpa-live-protocol-20261012")
			}
		},
		Transport:     traffic.UpstreamRequestCompression(upstreamTransport, *upstreamCompression),
		FlushInterval: -1,
		ModifyResponse: func(response *http.Response) error {
			if *nativeMuse {
				response.Body = newResponseObserver(response.Body, response.StatusCode, strings.Contains(response.Header.Get("Content-Type"), "text/event-stream"), func(meta map[string]any) {
					meta["event"] = "live_upstream_response"
					_ = json.NewEncoder(os.Stdout).Encode(meta)
				})
			} else {
				response.Body = observeResponse(response.Body, response.StatusCode, response.Header.Get("Content-Type"))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "live test upstream transport failed", http.StatusBadGateway)
		},
	}
	server := &http.Server{Addr: *listen, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if downstream != nil && r.Method == http.MethodPost && r.URL.Path == "/v1/responses" && r.Header.Get("Authorization") == "Bearer wire-client-test-only" {
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2*1024*1024))
			if err != nil || !json.Valid(raw) || gjson.GetBytes(raw, "model").String() != *prefix+"/"+*model || observed.Add(1) > *budget {
				http.Error(w, "downstream observation outside test scope", http.StatusBadRequest)
				return
			}
			meta := responsesRequestMetadata(raw)
			meta["event"] = "live_codex_request"
			meta["request"] = observed.Load()
			_ = json.NewEncoder(os.Stdout).Encode(meta)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			r.ContentLength = int64(len(raw))
			downstream.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1"+upstreamPath || r.Header.Get("Authorization") != "Bearer wire-upstream-test-only" {
			http.Error(w, "test request rejected", http.StatusForbidden)
			return
		}
		raw, wireBytes, encoding, err := readTestRequest(w, r)
		if err != nil || !json.Valid(raw) || gjson.GetBytes(raw, "model").String() != *model {
			http.Error(w, "model or payload outside test scope", http.StatusBadRequest)
			return
		}
		if admitted.Add(1) > *budget {
			http.Error(w, "live test request budget exhausted", http.StatusTooManyRequests)
			return
		}
		// Metadata only: no secret, request text, tool arguments or model output.
		names := []string{}
		for _, tool := range gjson.GetBytes(raw, "tools").Array() {
			name := tool.Get("function.name").String()
			if name == "" {
				name = tool.Get("name").String()
			}
			names = append(names, name)
		}
		meta := requestMetadata(raw)
		if *nativeMuse {
			meta = responsesRequestMetadata(raw)
			calls, outputs := 0, 0
			for _, item := range gjson.GetBytes(raw, "input").Array() {
				switch item.Get("type").String() {
				case "function_call":
					calls++
				case "function_call_output":
					outputs++
				}
			}
			meta["history_tool_calls"], meta["history_tool_results"] = calls, outputs
			meta["endpoint"] = "responses"
		}
		meta["event"] = "live_upstream_request"
		meta["request"] = admitted.Load()
		meta["model"] = *model
		meta["stream"] = gjson.GetBytes(raw, "stream").Bool()
		meta["tool_names"] = names
		meta["message_count"] = gjson.GetBytes(raw, "messages.#").Int()
		meta["request_identity_bytes"], meta["request_wire_bytes"], meta["request_content_encoding"] = len(raw), wireBytes, encoding
		meta["provider_request_compression"] = *upstreamCompression
		_ = json.NewEncoder(os.Stdout).Encode(meta)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
		proxy.ServeHTTP(w, r)
	})}
	_, _ = fmt.Fprintln(os.Stdout, "bounded live broker ready")
	return server.ListenAndServe()
}

func readTestRequest(w http.ResponseWriter, r *http.Request) ([]byte, int, string, error) {
	wire, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		return nil, 0, "", err
	}
	coding := r.Header.Get("Content-Encoding")
	body := wire
	if coding != "" && coding != "identity" {
		d, err := traffic.Decoder(bytes.NewReader(wire), coding)
		if err != nil {
			return nil, len(wire), coding, err
		}
		body, err = io.ReadAll(io.LimitReader(d, (2<<20)+1))
		_ = d.Close()
		if err != nil || len(body) > 2<<20 {
			return nil, len(wire), coding, fmt.Errorf("invalid compressed test body")
		}
		r.Header.Del("Content-Encoding")
	}
	return body, len(wire), coding, nil
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "live broker:", err)
		os.Exit(1)
	}
}
