package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

// Only protocol shape and usage are retained. Content, reasoning text, tool
// argument values, headers and credentials are never written to the audit log.
func requestMetadata(raw []byte) map[string]any {
	calls, outputs, reasoning := 0, 0, 0
	missing := 0
	pending := map[string]bool{}
	for _, message := range gjson.GetBytes(raw, "messages").Array() {
		if message.Get("reasoning_content").String() != "" {
			reasoning++
		}
		for _, call := range message.Get("tool_calls").Array() {
			calls++
			pending[call.Get("id").String()] = true
		}
		if message.Get("role").String() == "tool" {
			outputs++
			id := message.Get("tool_call_id").String()
			if !pending[id] {
				missing++
			}
			delete(pending, id)
		}
	}
	return map[string]any{"history_tool_calls": calls, "history_tool_results": outputs,
		"unanswered_history_calls": len(pending), "orphan_history_results": missing,
		"reasoning_messages": reasoning}
}

func responsesRequestMetadata(raw []byte) map[string]any {
	tools := []map[string]string{}
	var collect func(gjson.Result, string)
	collect = func(list gjson.Result, namespace string) {
		for _, tool := range list.Array() {
			name, kind := tool.Get("name").String(), tool.Get("type").String()
			if !safeLabel(name) || !safeLabel(kind) {
				continue
			}
			tools = append(tools, map[string]string{"name": name, "type": kind, "namespace": namespace})
			if kind == "namespace" {
				collect(tool.Get("tools"), name)
			}
		}
	}
	collect(gjson.GetBytes(raw, "tools"), "")
	types := map[string]bool{}
	for _, item := range gjson.GetBytes(raw, "input").Array() {
		kind := item.Get("type").String()
		if kind == "" && item.Get("role").Exists() {
			kind = "message"
		}
		if safeLabel(kind) {
			types[kind] = true
		}
	}
	return map[string]any{"tools": tools, "input_item_types": sortedLabels(types)}
}

type responseObserver struct {
	body      io.ReadCloser
	sse       bool
	status    int
	pending   []byte
	dropping  bool
	overLimit int
	frames    int
	models    map[string]bool
	finishes  map[string]bool
	names     map[string]bool
	calls     map[string]bool
	reasoning int
	usage     map[string]int64
	done      bool
	events    map[string]bool
	once      sync.Once
	writeLog  func(map[string]any)
}

func observeResponse(body io.ReadCloser, status int, contentType string) io.ReadCloser {
	return newResponseObserver(body, status, strings.Contains(contentType, "text/event-stream"), func(meta map[string]any) {
		_ = json.NewEncoder(os.Stdout).Encode(meta)
	})
}

func newResponseObserver(body io.ReadCloser, status int, sse bool, emit func(map[string]any)) *responseObserver {
	return &responseObserver{body: body, sse: sse, status: status, models: map[string]bool{},
		finishes: map[string]bool{}, names: map[string]bool{}, calls: map[string]bool{}, events: map[string]bool{}, writeLog: emit}
}

func (o *responseObserver) Read(p []byte) (int, error) {
	n, err := o.body.Read(p)
	for _, b := range p[:n] {
		if o.sse && b == '\n' {
			if !o.dropping {
				o.frame(bytes.TrimPrefix(bytes.TrimSpace(o.pending), []byte("data: ")))
			}
			o.pending, o.dropping = o.pending[:0], false
			continue
		}
		if !o.dropping {
			if len(o.pending) >= 64*1024 {
				o.dropping = true
				o.overLimit++
				o.pending = o.pending[:0]
			} else {
				o.pending = append(o.pending, b)
			}
		}
	}
	if err != nil {
		o.finish(err == io.EOF)
	}
	return n, err
}

func (o *responseObserver) Close() error {
	err := o.body.Close()
	o.finish(false)
	return err
}

func safeLabel(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-/.:", c)) {
			return false
		}
	}
	return true
}

func (o *responseObserver) frame(raw []byte) {
	if bytes.Equal(raw, []byte("[DONE]")) {
		o.done = true
		return
	}
	if !json.Valid(raw) {
		return
	}
	o.frames++
	data := gjson.ParseBytes(raw)
	if kind := data.Get("type").String(); safeLabel(kind) {
		o.events[kind] = true
	}
	for _, item := range []gjson.Result{data.Get("item"), data.Get("output_item")} {
		if kind := item.Get("type").String(); kind == "function_call" || kind == "custom_tool_call" {
			name := item.Get("name").String()
			if namespace := item.Get("namespace").String(); safeLabel(namespace) {
				name = namespace + "." + name
			}
			if safeLabel(name) {
				o.names[name] = true
			}
			if id := item.Get("call_id").String(); safeLabel(id) {
				o.calls[id] = true
			}
		}
	}
	if response := data.Get("response"); response.IsObject() {
		if model := response.Get("model").String(); safeLabel(model) {
			o.models[model] = true
		}
		if usage := response.Get("usage"); usage.IsObject() {
			o.usage = map[string]int64{}
			for _, name := range []string{"input_tokens", "output_tokens", "total_tokens", "input_tokens_details.cached_tokens", "output_tokens_details.reasoning_tokens"} {
				if value := usage.Get(name); value.Type == gjson.Number {
					o.usage[name] = value.Int()
				}
			}
		}
	}
	if model := data.Get("model").String(); safeLabel(model) {
		o.models[model] = true
	}
	for _, choice := range data.Get("choices").Array() {
		if reason := choice.Get("finish_reason").String(); safeLabel(reason) {
			o.finishes[reason] = true
		}
		for _, message := range []gjson.Result{choice.Get("delta"), choice.Get("message")} {
			o.reasoning += len(message.Get("reasoning_content").String())
			for _, call := range message.Get("tool_calls").Array() {
				if name := call.Get("function.name").String(); safeLabel(name) {
					o.names[name] = true
				}
				if id := call.Get("id").String(); safeLabel(id) {
					o.calls[id] = true
				}
			}
		}
	}
	if usage := data.Get("usage"); usage.IsObject() {
		o.usage = map[string]int64{}
		fields := []string{"prompt_tokens", "completion_tokens", "total_tokens", "prompt_tokens_details.cached_tokens", "completion_tokens_details.reasoning_tokens"}
		if data.Get("object").String() == "response" {
			fields = []string{"input_tokens", "output_tokens", "total_tokens", "input_tokens_details.cached_tokens", "output_tokens_details.reasoning_tokens"}
		}
		for _, name := range fields {
			if value := usage.Get(name); value.Type == gjson.Number {
				o.usage[name] = value.Int()
			}
		}
	}
}

func sortedLabels(values map[string]bool) []string {
	items := make([]string, 0, len(values))
	for value := range values {
		items = append(items, value)
	}
	sort.Strings(items)
	return items
}

func (o *responseObserver) finish(eof bool) {
	o.once.Do(func() {
		if !o.dropping && len(o.pending) > 0 {
			o.frame(bytes.TrimPrefix(bytes.TrimSpace(o.pending), []byte("data: ")))
		}
		event := "live_upstream_response"
		if len(o.events) > 0 {
			event = "live_codex_response"
		}
		o.writeLog(map[string]any{"event": event, "status": o.status,
			"sse": o.sse, "eof": eof, "done_marker": o.done, "json_frames": o.frames,
			"oversize_observer_frames": o.overLimit, "reported_models": sortedLabels(o.models),
			"finish_reasons": sortedLabels(o.finishes), "tool_names": sortedLabels(o.names),
			"tool_call_count": len(o.calls), "reasoning_bytes": o.reasoning, "usage": o.usage,
			"response_event_types": sortedLabels(o.events)})
	})
}
