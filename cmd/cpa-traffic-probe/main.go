// cpa-traffic-probe executes two bounded Responses turns in an isolated test
// network. It never accepts an Internet URL or a production credential.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/traffic"
	"github.com/tidwall/gjson"
)

type evidence struct {
	IdentityUpload   int             `json:"identity_upload_bytes"`
	WireUpload       int             `json:"wire_upload_bytes"`
	IdentityDownload int             `json:"identity_download_bytes"`
	WireDownload     int             `json:"wire_download_bytes"`
	Status           int             `json:"status"`
	FirstEventMS     int64           `json:"first_event_ms"`
	CompletedMS      int64           `json:"completed_ms"`
	Usage            json.RawMessage `json:"usage"`
}

func turn(client *http.Client, endpoint string, body []byte) (gjson.Result, evidence, error) {
	e := evidence{IdentityUpload: len(body)}
	var wire bytes.Buffer
	z, _ := traffic.Encoder(&wire, "gzip")
	_, _ = z.Write(body)
	_ = z.Close()
	e.WireUpload = wire.Len()
	r, _ := http.NewRequest("POST", endpoint, bytes.NewReader(wire.Bytes()))
	r.Header.Set("Authorization", "Bearer wire-client-test-only")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Encoding", "gzip")
	r.Header.Set("Accept-Encoding", "gzip")
	start := time.Now()
	response, err := client.Do(r)
	if err != nil {
		return gjson.Result{}, e, err
	}
	defer response.Body.Close()
	e.Status = response.StatusCode
	encoded := &countReader{r: response.Body}
	var reader io.Reader = encoded
	if coding := response.Header.Get("Content-Encoding"); coding != "" {
		d, err := traffic.Decoder(encoded, coding)
		if err != nil {
			return gjson.Result{}, e, err
		}
		defer d.Close()
		reader = d
	}
	all, err := io.ReadAll(io.TeeReader(io.LimitReader(reader, (4<<20)+1), firstEventWriter{at: &e.FirstEventMS, start: start}))
	e.CompletedMS = time.Since(start).Milliseconds()
	e.WireDownload = encoded.n
	e.IdentityDownload = len(all)
	if err != nil || len(all) > 4<<20 {
		return gjson.Result{}, e, fmt.Errorf("invalid or oversized response")
	}
	if e.Status != 200 {
		return gjson.Result{}, e, fmt.Errorf("test upstream status %d", e.Status)
	}
	for _, line := range strings.Split(string(all), "\n") {
		if strings.HasPrefix(line, "data:") {
			event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			if event.Get("type").String() == "response.completed" {
				result := event.Get("response")
				e.Usage = json.RawMessage(result.Get("usage").Raw)
				return result, e, nil
			}
		}
	}
	return gjson.Result{}, e, fmt.Errorf("stream ended without response.completed")
}

type countReader struct {
	r io.Reader
	n int
}

func (r *countReader) Read(b []byte) (int, error) { n, err := r.r.Read(b); r.n += n; return n, err }

type firstEventWriter struct {
	at    *int64
	start time.Time
}

func (w firstEventWriter) Write(b []byte) (int, error) {
	if *w.at == 0 && len(b) > 0 {
		*w.at = time.Since(w.start).Milliseconds() + 1
	}
	return len(b), nil
}

func run() error {
	base := flag.String("base", "", "Private cpa-live-go-* HTTP test gateway")
	model := flag.String("model", "1x/gpt-6.1-sol", "Explicit existing test subscription")
	flag.Parse()
	u, err := url.Parse(*base)
	if err != nil || u.Scheme != "http" || !strings.HasPrefix(u.Hostname(), "cpa-live-go-") || u.Port() != "8317" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return fmt.Errorf("isolated test gateway required")
	}
	if *model != "1x/gpt-6.1-sol" {
		if *model == "opencode/muse-spark-1.3-contributor" {
			return runMuseProbe(*base)
		}
		return fmt.Errorf("unapproved test model")
	}
	tools := []any{map[string]any{"type": "function", "name": "lookup", "description": "Returns value multiplied by six", "parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "integer"}}, "required": []string{"value"}, "additionalProperties": false}}}
	prompt := "Bounded transport protocol test. Ignore the filler context. Call lookup once with value 7. Do not answer until the tool result. After the result, reply with the single number returned. Filler: " + strings.Repeat("ignore-filler-", 1200)
	first, _ := json.Marshal(map[string]any{"model": *model, "stream": true, "input": []any{map[string]any{"role": "user", "content": prompt}}, "tools": tools, "tool_choice": "required"})
	transport := &http.Transport{Proxy: nil, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	result, a, err := turn(client, *base+"/v1/responses", first)
	if err != nil {
		return err
	}
	callID := ""
	for _, item := range result.Get("output").Array() {
		if item.Get("type").String() == "function_call" && item.Get("name").String() == "lookup" {
			if item.Get("arguments").Get("value").Int() != 7 {
				args := gjson.Parse(item.Get("arguments").String())
				if args.Get("value").Int() != 7 {
					return fmt.Errorf("unexpected tool arguments")
				}
			}
			callID = item.Get("call_id").String()
		}
	}
	if callID == "" || result.Get("id").String() == "" {
		return fmt.Errorf("upstream did not return required tool call")
	}
	// Execute the declared deterministic operation locally, not arbitrary code.
	toolOutput := 7 * 6
	second, _ := json.Marshal(map[string]any{"model": *model, "stream": true, "previous_response_id": result.Get("id").String(), "input": []any{map[string]any{"type": "function_call_output", "call_id": callID, "output": fmt.Sprint(toolOutput)}}, "tools": tools, "tool_choice": "none"})
	final, b, err := turn(client, *base+"/v1/responses", second)
	if err != nil {
		return err
	}
	text := ""
	for _, item := range final.Get("output").Array() {
		for _, part := range item.Get("content").Array() {
			if part.Get("type").String() == "output_text" {
				text += part.Get("text").String()
			}
		}
	}
	if strings.TrimSpace(text) != "42" {
		return fmt.Errorf("tool continuation result was incorrect")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "model": *model, "real_requests": 2, "tool_call_executed": true, "tool_result_verified": true, "full": a, "incremental": b, "banked_reset_used": false, "production_replaced": false})
}

func main() {
	if err := run(); err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": err.Error()})
		os.Exit(1)
	}
}
