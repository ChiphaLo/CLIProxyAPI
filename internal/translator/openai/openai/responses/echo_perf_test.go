package responses

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func echoChunk(content string, extra string) []byte {
	return []byte(`data: {"id":"chatcmpl-1","created":1700000000,"object":"chat.completion.chunk","choices":[{"index":0,"delta":{` + extra + `}}]}`)
}

func runEchoStream(request []byte) []byte {
	chunks := [][]byte{
		echoChunk("", `"role":"assistant","content":""`),
		echoChunk("", `"reasoning_content":"think"`),
		echoChunk("", `"content":"Hel"`),
		echoChunk("", `"content":"lo"`),
		[]byte(`data: {"id":"chatcmpl-1","created":1700000000,"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"editor__read","arguments":"{\"path\":\"/tmp/x\"}"}}]}}]}`),
		[]byte(`data: {"id":"chatcmpl-1","created":1700000000,"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`),
		[]byte(`data: [DONE]`),
	}
	var param any
	var out []byte
	for _, chunk := range chunks {
		out = append(out, bytes.Join(ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "test", request, request, chunk, &param), nil)...)
	}
	return out
}

func echoRequests() []struct {
	name string
	raw  []byte
} {
	history := strings.Repeat("context ", 4096)
	tools := `[{"type":"namespace","name":"editor","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}]`
	return []struct {
		name string
		raw  []byte
	}{
		{"nil", nil},
		{"empty-object", []byte(`{}`)},
		{"malformed", []byte(`{`)},
		{"full", []byte(`{"model":"gpt-5","instructions":"` + history + `","max_output_tokens":4096,"max_tool_calls":3,"parallel_tool_calls":false,"previous_response_id":"resp_prev","prompt_cache_key":"k1","reasoning":{"effort":"high","summary":"auto"},"safety_identifier":"user-1","service_tier":"priority","store":true,"temperature":1,"text":{"format":{"type":"json_schema","name":"x"}},"tool_choice":"auto","tools":` + tools + `,"top_logprobs":5,"top_p":0.5,"truncation":"auto","user":"u1","metadata":{"b":2,"a":1},"input":[{"role":"user","content":"hi"}]}`)},
		{"weird-types", []byte(`{"model":42,"instructions":null,"temperature":"hot","store":"yes","parallel_tool_calls":1,"max_output_tokens":"999","top_p":2,"tools":"none","metadata":[1,2],"user":{"id":7},"input":[]}`)},
		{"duplicate-keys", []byte(`{"model":"first","input":[],"model":"second","temperature":0.1,"temperature":0.9}`)},
		{"escaped-key", []byte("{\"mod\\u0065l\":\"esc-model\",\"input\":[],\"tools\":" + tools + "}")},
		{"max-tokens-only", []byte(`{"model":"gpt-5","max_tokens":777,"input":[]}`)},
	}
}

// Golden hashes lock request echo, namespace resolution and message phases.
var echoGoldenHashes = map[string][2]string{
	"nil":             {"a7e7e46154e1d529727e60346fd45e3e25e7f3ab267aff7d5c9d0167c8b0bc56", "3043cc28af7b69b601ff99c395fe276195e3c7ab51ba9c468e00e2012ec8c9ce"},
	"empty-object":    {"a7e7e46154e1d529727e60346fd45e3e25e7f3ab267aff7d5c9d0167c8b0bc56", "3043cc28af7b69b601ff99c395fe276195e3c7ab51ba9c468e00e2012ec8c9ce"},
	"malformed":       {"a7e7e46154e1d529727e60346fd45e3e25e7f3ab267aff7d5c9d0167c8b0bc56", "3043cc28af7b69b601ff99c395fe276195e3c7ab51ba9c468e00e2012ec8c9ce"},
	"full":            {"c60f6b60e8735c3329fbe1bf8f7fa9c100c8f8ce129054fca923bb98c64e6edb", "9df922e160f455f8310491cf0192d456c377170717d9a6e06c79a0b38a623f5a"},
	"weird-types":     {"d432d204b7bd1a91331cf2da316d9303f7cf36a820696dacc87adf163e5361e3", "519aa2d67a36dde2184b955a5b3b6549d6ef0cc7a143a88bbe35ef429fc15972"},
	"duplicate-keys":  {"ffd0162ed6a0b7d4081da30511ac29a4e61368d434b5eab775e981f978665a04", "c6e59c4303d0f26fb9f6187bbf2301a0be79d514fe8edf98af4b56ef53721080"},
	"escaped-key":     {"694eafbe419de31486592f2ef882a74bb109573ed331c4e04d948863616b3c78", "a8004142a72f950473d676e3d3998e6a7706a4a333b72249ff017b8a2176451c"},
	"max-tokens-only": {"417b39792cb22deeb361d8190ca9fd3bfe34ece3e7e2c89fcaac9017c88331e0", "d30ef175849d1e1e266bcddebb58722acbd9fe15c710cabfa40ba21811705781"},
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestResponsesEchoGoldenOutputs(t *testing.T) {
	for _, tc := range echoRequests() {
		want := echoGoldenHashes[tc.name]
		first := runEchoStream(tc.raw)
		if got := hashHex(first); got != want[0] {
			t.Errorf("%s stream: hash %s, want %s", tc.name, got, want[0])
		}
		resp := []byte(`{"id":"chatcmpl-9","created":1700000000,"model":"resp-model","choices":[{"message":{"role":"assistant","content":"done","reasoning_content":"why","tool_calls":[{"id":"call_9","type":"function","function":{"name":"editor__read","arguments":"{\"path\":\"/tmp/y\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
		var p1 any
		n1 := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "test", tc.raw, tc.raw, resp, &p1)
		if got := hashHex(n1); got != want[1] {
			t.Errorf("%s nonstream: hash %s, want %s", tc.name, got, want[1])
		}
	}
}

func BenchmarkResponsesEchoShortStreams(b *testing.B) {
	request := responsesPerfRequest(100)
	chunk := echoChunk("", `"content":"x"`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var param any
		ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "test", request, request, chunk, &param)
		ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "test", request, request, []byte("data: [DONE]"), &param)
	}
}

// Golden hashes for root-shape and reasoning-inclusion edge cases, also
// covering native final-answer phases.
func TestResponsesEchoGoldenEdgeCases(t *testing.T) {
	for name, req := range map[string][]byte{
		"array-root":  []byte(`[{"model":"x"}]`),
		"scalar-root": []byte(`"hello"`),
		"number-root": []byte(`42`),
	} {
		if got := hashHex(runEchoStream(req)); got != "a7e7e46154e1d529727e60346fd45e3e25e7f3ab267aff7d5c9d0167c8b0bc56" {
			t.Errorf("%s stream: hash %s", name, got)
		}
		resp := []byte(`{"id":"c1","created":1700000000,"model":"m","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		var p any
		if got := hashHex(ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "test", req, req, resp, &p)); got != "e3ee4b75ffb80ad7bb7723942645b3a1bc7d0439daa75fe3a9207818f96b93a5" {
			t.Errorf("%s nonstream: hash %s", name, got)
		}
	}
	respNoRC := []byte(`{"id":"c2","created":1700000000,"model":"m","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	want := map[string]string{
		"req-with-reasoning":    "046cac1c6858e2a1c8789ec4e80c6f0c78fbd5d1a8876ca087dff4f84c4e2189",
		"req-without-reasoning": "1efd8ec48812a93548a776dccfe47d1955a78355c81e5c93fdd7102c47b8c798",
		"req-reasoning-null":    "c5ae576bde100e2490519770b81a84dadba78888e0950980df80fa5926d39c72",
	}
	for name, req := range map[string][]byte{
		"req-with-reasoning":    []byte(`{"model":"gpt-5","reasoning":{"effort":"low"},"input":[]}`),
		"req-without-reasoning": []byte(`{"model":"gpt-5","input":[]}`),
		"req-reasoning-null":    []byte(`{"model":"gpt-5","reasoning":null,"input":[]}`),
	} {
		var p any
		if got := hashHex(ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "test", req, req, respNoRC, &p)); got != want[name] {
			t.Errorf("%s nonstream: hash %s, want %s", name, got, want[name])
		}
	}
}
