// cpa-wire-probe is an offline upstream simulator and loopback-only verifier.
// Returned tool arguments are data; this program never executes any tool call.
package main

import (
	"bufio"
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

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/opencode"
	"github.com/tidwall/gjson"
)

func writeData(w http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func mock(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
	if err != nil || !gjson.ValidBytes(data) {
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return
	}
	name := gjson.GetBytes(data, "tools.0.function.name").String()
	tool := name != "" && gjson.GetBytes(data, "tool_choice").String() != "none"
	usage := map[string]any{"prompt_tokens": 1001, "completion_tokens": 137, "total_tokens": 1138,
		"prompt_tokens_details":     map[string]any{"cached_tokens": 701, "cache_write_tokens": 23},
		"completion_tokens_details": map[string]any{"reasoning_tokens": 100}}
	message := map[string]any{"role": "assistant", "content": "WIRE_PROBE_OK"}
	finish := "stop"
	call := map[string]any{"index": 0, "id": "call_wire_fixture", "type": "function",
		"function": map[string]any{"name": name, "arguments": `{"input":"text(1);"}`}}
	if tool {
		message["content"] = nil
		message["tool_calls"] = []any{call}
		finish = "tool_calls"
	}
	dsmlCase := ""
	for _, candidate := range []string{"valid", "invalid", "truncated"} {
		if strings.Contains(gjson.GetBytes(data, "messages").Raw, "DSML_CASE="+candidate) {
			dsmlCase = candidate
		}
	}
	dsmlText := ""
	if tool && dsmlCase != "" {
		invoke := `<｜｜DSML｜｜ invoke name="` + name + `"><｜｜DSML｜｜ parameter name="input" string="true">text(1);</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke>`
		if dsmlCase == "invalid" {
			invoke += `<｜｜DSML｜｜ invoke name="undeclared"><｜｜DSML｜｜ parameter name="input" string="true">not-authorized</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke>`
		}
		dsmlText = `<｜｜DSML｜｜ calls>` + invoke + `</｜｜DSML｜｜ calls>`
		finish = "stop"
		if dsmlCase == "truncated" {
			dsmlText = strings.TrimSuffix(dsmlText, `</｜｜DSML｜｜ calls>`)
			finish = "length"
		}
		message["content"] = dsmlText
		delete(message, "tool_calls")
	}
	if !gjson.GetBytes(data, "stream").Bool() {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl_wire_fixture", "object": "chat.completion", "model": "wire-reported-model",
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if dsmlText != "" {
		// Deliberately split both the DSML root and the tool name across frames.
		characters := []rune(dsmlText)
		for start := 0; start < len(characters); start += 7 {
			end := start + 7
			if end > len(characters) {
				end = len(characters)
			}
			var terminal any
			if end == len(characters) && dsmlCase != "truncated" {
				terminal = finish
			}
			writeData(w, map[string]any{"id": "chatcmpl_wire_fixture", "object": "chat.completion.chunk", "model": "wire-reported-model", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": string(characters[start:end])}, "finish_reason": terminal}}})
		}
		writeData(w, map[string]any{"id": "chatcmpl_wire_fixture", "object": "chat.completion.chunk", "model": "wire-reported-model", "choices": []any{}, "usage": usage})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	delta := map[string]any{"role": "assistant", "content": "WIRE_PROBE_OK"}
	if tool {
		delta = map[string]any{"tool_calls": []any{call}}
	}
	writeData(w, map[string]any{"id": "chatcmpl_wire_fixture", "object": "chat.completion.chunk", "model": "wire-reported-model",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	writeData(w, map[string]any{"id": "chatcmpl_wire_fixture", "object": "chat.completion.chunk", "model": "wire-reported-model", "choices": []any{}, "usage": usage})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

type result struct {
	Scenario string `json:"scenario"`
	Passed   bool   `json:"passed"`
	Events   int    `json:"events,omitempty"`
}

func check(base, keyPath, requestedModel string, additional bool, dsmlCase string) ([]result, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil {
		return nil, fmt.Errorf("verification is restricted to explicit HTTP loopback URLs")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 10 * time.Second}
	results := []result{}
	for _, stream := range []bool{false, true} {
		for _, tool := range []bool{false, true} {
			body := map[string]any{"model": requestedModel, "stream": stream,
				"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "offline fixture DSML_CASE=" + dsmlCase}}}}}
			if tool {
				declarations := []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{map[string]any{"type": "custom", "name": "exec"}}}}
				if additional {
					body["input"] = append([]any{map[string]any{"type": "additional_tools", "role": "developer", "tools": declarations}}, body["input"].([]any)...)
				} else {
					body["tools"] = declarations
				}
			}
			data, _ := json.Marshal(body)
			req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/v1/responses", bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
			req.Header.Set("Content-Type", "application/json")
			response, err := client.Do(req)
			if err != nil {
				return nil, err
			}
			if response.StatusCode != http.StatusOK {
				_ = response.Body.Close()
				return nil, fmt.Errorf("fixture returned HTTP %d", response.StatusCode)
			}
			var completed []byte
			events, terminals := 0, 0
			toolEvents := 0
			if stream {
				scanner := bufio.NewScanner(response.Body)
				scanner.Buffer(make([]byte, 16*1024), 1024*1024)
				for scanner.Scan() {
					line := scanner.Text()
					if strings.HasPrefix(line, "data: ") {
						event := []byte(strings.TrimPrefix(line, "data: "))
						events++
						if kind := gjson.GetBytes(event, "item.type").String(); kind == "custom_tool_call" || kind == "function_call" {
							toolEvents++
						}
						if gjson.GetBytes(event, "type").String() == "response.completed" {
							terminals++
							completed = []byte(gjson.GetBytes(event, "response").Raw)
						}
					}
				}
				if err := scanner.Err(); err != nil {
					_ = response.Body.Close()
					return nil, err
				}
				if terminals != 1 {
					_ = response.Body.Close()
					return nil, fmt.Errorf("expected one terminal event, got %d", terminals)
				}
			} else {
				completed, err = io.ReadAll(io.LimitReader(response.Body, 1024*1024))
				if err != nil {
					_ = response.Body.Close()
					return nil, err
				}
			}
			_ = response.Body.Close()
			if gjson.GetBytes(completed, "usage.input_tokens").Int() != 1001 || gjson.GetBytes(completed, "usage.output_tokens").Int() != 137 || gjson.GetBytes(completed, "usage.input_tokens_details.cached_tokens").Int() != 701 {
				return nil, fmt.Errorf("fixture usage lost (stream=%t tool=%t)", stream, tool)
			}
			if tool && (dsmlCase == "invalid" || dsmlCase == "truncated") {
				if toolEvents > 0 || !strings.Contains(string(completed), opencode.FormatError) {
					return nil, fmt.Errorf("invalid DSML was not rejected atomically (stream=%t)", stream)
				}
				for _, item := range gjson.GetBytes(completed, "output").Array() {
					if kind := item.Get("type").String(); kind == "custom_tool_call" || kind == "function_call" {
						return nil, fmt.Errorf("invalid DSML leaked a call")
					}
				}
			} else if tool {
				if gjson.GetBytes(completed, "output.0.type").String() != "custom_tool_call" || gjson.GetBytes(completed, "output.0.namespace").String() != "functions" || gjson.GetBytes(completed, "output.0.name").String() != "exec" || gjson.GetBytes(completed, "output.0.input").String() != "text(1);" {
					return nil, fmt.Errorf("fixture tool identity lost (stream=%t)", stream)
				}
			} else if !strings.Contains(string(completed), "WIRE_PROBE_OK") {
				return nil, fmt.Errorf("fixture text lost")
			}
			results = append(results, result{Scenario: fmt.Sprintf("stream=%t tool=%t additional=%t dsml=%s", stream, tool, additional, dsmlCase), Passed: true, Events: events})
		}
	}
	return results, nil
}

func run() error {
	mode := flag.String("mode", "check", "mock or check")
	listen := flag.String("listen", "127.0.0.1:18330", "Simulator listener")
	base := flag.String("base", "", "Loopback test CPA URL")
	keyPath := flag.String("key-file", "", "Test-only client key file")
	model := flag.String("model", "fixture/wire-fixture", "Test fixture model")
	receiptsPath := flag.String("receipts-file", "", "Synthetic receipt file to verify")
	expected := flag.Int("expected-rows", 8, "Expected synthetic receipts")
	additional := flag.Bool("additional-tools", false, "Use developer additional_tools records")
	dsmlCase := flag.String("dsml-case", "", "Synthetic DSML scenario: valid, invalid or truncated")
	flag.Parse()
	if *dsmlCase != "" && *dsmlCase != "valid" && *dsmlCase != "invalid" && *dsmlCase != "truncated" {
		return fmt.Errorf("unknown synthetic DSML scenario")
	}
	if *mode == "receipts" {
		file, err := os.Open(*receiptsPath)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		scanner := bufio.NewScanner(file)
		count := 0
		for scanner.Scan() {
			row, err := billing.DecodeRow(scanner.Bytes())
			if err != nil || row.Model != "opencode/wire-fixture" || row.KeyID != "admin" || row.Input != 1001 || row.Cached != 701 || row.Write != 23 || row.Output != 137 || row.UpstreamReportedModel != "wire-reported-model" || !row.ModelMismatch || row.ClientIP != "127.0.0.1" {
				return fmt.Errorf("synthetic receipt %d differs from the legacy contract", count+1)
			}
			amount := billing.Cost(row, map[string]*billing.Rates{"wire-fixture": {Input: .1, Cached: .01, Write: .125, Output: .5}})
			if amount != .00010838 {
				return fmt.Errorf("synthetic receipt fee differs from the legacy formula")
			}
			count++
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		if count != *expected {
			return fmt.Errorf("synthetic receipt count %d, expected %d", count, *expected)
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"receipts": count, "tokens_and_prices": "legacy-parity", "response_model": "upstream-metadata", "passed": true})
	}
	if *mode == "mock" {
		server := http.Server{Addr: *listen, Handler: http.HandlerFunc(mock), ReadHeaderTimeout: 5 * time.Second}
		return server.ListenAndServe()
	}
	if *mode != "check" {
		return fmt.Errorf("invalid probe mode")
	}
	results, err := check(*base, *keyPath, *model, *additional, *dsmlCase)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(results)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "wire probe failed:", err)
		os.Exit(1)
	}
}
