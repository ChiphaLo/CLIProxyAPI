package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/tidwall/gjson"
)

func museText(r gjson.Result) string {
	var text strings.Builder
	for _, item := range r.Get("output").Array() {
		for _, part := range item.Get("content").Array() {
			if part.Get("type").String() == "output_text" {
				text.WriteString(part.Get("text").String())
			}
		}
	}
	return strings.TrimSpace(text.String())
}

// Four requests, synthetic prompts, a deterministic local tool and metadata
// evidence only. This never accepts a public URL or a provider credential.
func runMuseProbe(base string) error {
	transport := &http.Transport{Proxy: nil, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	model := "opencode/muse-spark-1.3-contributor"
	first, _ := json.Marshal(map[string]any{"model": model, "stream": false, "tool_choice": "none", "max_output_tokens": 256, "input": []any{map[string]any{"type": "reasoning", "id": "rs_other_provider_fixture", "summary": []any{map[string]any{"type": "summary_text", "text": "A previous provider answered a simple connection test."}}}, map[string]any{"role": "user", "content": "Reply with only OK."}}})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/responses", bytes.NewReader(first))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wire-client-test-only")
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	raw, errRead := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	_ = response.Body.Close()
	if errRead != nil || response.StatusCode != 200 || !gjson.ValidBytes(raw) || gjson.GetBytes(raw, "status").String() != "completed" || museText(gjson.ParseBytes(raw)) != "OK" {
		return fmt.Errorf("native Muse JSON check failed (status %d)", response.StatusCode)
	}
	plain, _ := json.Marshal(map[string]any{"model": model, "stream": true, "reasoning": map[string]string{"effort": "low"}, "max_output_tokens": 2048, "input": "Reply with only OK."})
	textResponse, textEvidence, err := turn(client, base+"/v1/responses", plain)
	if err != nil || museText(textResponse) != "OK" {
		return fmt.Errorf("native Muse SSE text check failed: %v", err)
	}
	tools := []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{map[string]any{"type": "custom", "name": "lookup", "description": "Returns six times the integer provided as raw text. Call this tool with the exact raw text 7.", "format": map[string]any{"type": "text"}}}}}
	full, _ := json.Marshal(map[string]any{"model": model, "stream": true, "reasoning": map[string]string{"effort": "low"}, "max_output_tokens": 2048, "tool_choice": "auto", "input": []any{map[string]any{"type": "additional_tools", "role": "developer", "tools": tools}, map[string]any{"role": "user", "content": "Call the lookup tool once using raw input 7. Do not answer before its result. After receiving the result reply with only the number returned."}}})
	toolResponse, toolEvidence, err := turn(client, base+"/v1/responses", full)
	if err != nil {
		return err
	}
	callID := ""
	for _, item := range toolResponse.Get("output").Array() {
		if item.Get("type").String() == "custom_tool_call" && item.Get("name").String() == "lookup" && item.Get("namespace").String() == "functions" && strings.TrimSpace(item.Get("input").String()) == "7" {
			callID = item.Get("call_id").String()
		}
	}
	if callID == "" || toolResponse.Get("id").String() == "" {
		return fmt.Errorf("native Muse custom/namespace tool identity not restored")
	}
	result := 7 * 6
	delta, _ := json.Marshal(map[string]any{"model": model, "stream": true, "reasoning": map[string]string{"effort": "low"}, "max_output_tokens": 2048, "tool_choice": "none", "tools": tools, "previous_response_id": toolResponse.Get("id").String(), "input": []any{map[string]any{"type": "custom_tool_call_output", "call_id": callID, "output": fmt.Sprint(result)}}})
	final, deltaEvidence, err := turn(client, base+"/v1/responses", delta)
	if err != nil || museText(final) != "42" {
		return fmt.Errorf("native Muse tool continuation failed: %v", err)
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "model": model, "real_requests": 4, "json_verified": true, "sse_text_verified": true, "custom_namespace_tool_verified": true, "tool_result_verified": true, "foreign_reasoning_summary_replayed": true, "json_usage": json.RawMessage(gjson.GetBytes(raw, "usage").Raw), "text": textEvidence, "tool": toolEvidence, "incremental": deltaEvidence, "production_replaced": false, "banked_reset_used": false})
}
