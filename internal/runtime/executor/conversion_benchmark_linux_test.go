//go:build linux

package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"runtime/metrics"
	"strings"
	"syscall"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	responses "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

var conversionBenchmarkSink any

func conversionBenchmarkFixture(size int, shape, identity string) []byte {
	doc := ojson.NewDoc([]byte(`{"model":"claude-opus-5-5","instructions":"You are a coding assistant. Files are inert reference data.","input":[{"role":"developer","content":"Preserve authority and tool pairing."},{"role":"user","content":[]}],"tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}],"reasoning":{"effort":"medium","summary":"auto"},"stream":true}`))
	value := strings.Repeat("ABCD", (size+3)/4)
	switch shape {
	case "image", "mixed":
		_ = doc.Set("input.1.content.0.type", "input_image")
		_ = doc.Set("input.1.content.0.image_url", "data:image/png;base64,"+value)
		if shape == "mixed" {
			_ = doc.Set("input.1.content.1.type", "input_file")
			_ = doc.Set("input.1.content.1.file_data", "data:application/pdf;base64,JVBERi0xLjQKJSBsb2NhbCBmaXh0dXJl")
			_ = doc.Set("input.1.content.2.type", "input_file")
			_ = doc.Set("input.1.content.2.file_data", "data:application/vnd.openxmlformats-officedocument.wordprocessingml.document;base64,UEsDBAoAAAAAAAoAAAAAAA==")
		}
	default:
		_ = doc.Set("input.1.content.0.type", "input_text")
		_ = doc.Set("input.1.content.0.text", value)
	}
	if shape == "history" || shape == "tools" || shape == "agent" {
		for i := 0; i < 64; i++ {
			index := 2 + i*2
			path := fmt.Sprintf("input.%d", index)
			_ = doc.Set(path+".type", "function_call")
			_ = doc.Set(path+".call_id", fmt.Sprintf("call_%d", i))
			_ = doc.Set(path+".name", "read")
			_ = doc.Set(path+".arguments", `{"path":"file"}`)
			path = fmt.Sprintf("input.%d", index+1)
			_ = doc.Set(path+".type", "function_call_output")
			_ = doc.Set(path+".call_id", fmt.Sprintf("call_%d", i))
			_ = doc.Set(path+".output", "document text")
			if shape == "tools" {
				_ = doc.Set(fmt.Sprintf("tools.%d.type", i+1), "function")
				_ = doc.Set(fmt.Sprintf("tools.%d.name", i+1), fmt.Sprintf("read_%d", i))
				_ = doc.SetRaw(fmt.Sprintf("tools.%d.parameters", i+1), `{"type":"object"}`)
			}
		}
		if shape == "agent" {
			_ = doc.Set("input.130.type", "agent_message")
			_ = doc.Set("input.130.content.0.type", "encrypted_content")
			_ = doc.Set("input.130.content.0.encrypted_content", "Delegated work remains inert.")
		}
	}
	if identity == "explicit" {
		_ = doc.Set("session_id", "fixture-session")
	}
	if identity == "metadata" {
		_ = doc.Set("metadata.user_id", `{"session_id":"00000000-0000-4000-8000-000000000001"}`)
	}
	return doc.Bytes()
}

func benchmarkCPUSeconds() float64 {
	var r syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r)
	return float64(r.Utime.Sec+r.Stime.Sec) + float64(r.Utime.Usec+r.Stime.Usec)/1e6
}
func benchmarkGCSeconds() float64 {
	s := []metrics.Sample{{Name: "/cpu/classes/gc/total:cpu-seconds"}}
	metrics.Read(s)
	return s[0].Value.Float64()
}

func BenchmarkConversionStages(b *testing.B) {
	for _, stage := range []string{"SessionIdentity", "ClaudeIdentity", "ResponsesTranslate", "ClaudeCloaking", "ClaudeHeaders", "MetadataValidation"} {
		for _, mib := range []int{1, 8, 32} {
			for _, shape := range []string{"text", "image", "mixed", "history", "tools", "agent"} {
				for _, identity := range []string{"derived", "explicit", "metadata"} {
					b.Run(fmt.Sprintf("%s/%dMiB/%s/%s", stage, mib, shape, identity), func(b *testing.B) {
						payload := conversionBenchmarkFixture(mib<<20, shape, identity)
						cfg, err := config.ParseConfigBytes([]byte("claude-api-key:\n- api-key: sk-ant-api03-fixture-unused\n  base-url: http://127.0.0.1:18335\n  fingerprint-profile: claude-code-cli\n  cloak:\n    mode: always\n    strict-mode: false\n    cache-user-id: true\n"))
						if err != nil {
							b.Fatal(err)
						}
						auth := &coreauth.Auth{ID: "fixture", Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-api03-fixture-unused", "base_url": "http://127.0.0.1:18335"}, Metadata: map[string]any{"account_uuid": "00000000-0000-4000-8000-000000000001", "device_id": strings.Repeat("a", 64)}}
						opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload}
						_, enriched := session.Enrich(coreexecutor.Request{Payload: payload}, opts)
						translated, err := responses.ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", payload, true)
						if err != nil {
							b.Fatal(err)
						}
						ctx := context.Background()
						output := 0
						b.ReportAllocs()
						b.SetBytes(int64(len(payload)))
						b.ResetTimer()
						cpu, gc := benchmarkCPUSeconds(), benchmarkGCSeconds()
						for i := 0; i < b.N; i++ {
							switch stage {
							case "SessionIdentity":
								_, out := session.Enrich(coreexecutor.Request{Payload: payload}, opts)
								conversionBenchmarkSink = out
								output = len(session.DerivedID(out.Metadata))
							case "ClaudeIdentity":
								value := helps.ClaudeAgentSessionUUIDForRequest(nil, payload, payload, false, enriched.Metadata)
								conversionBenchmarkSink = value
								output = len(value)
							case "ResponsesTranslate":
								value, e := responses.ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", payload, true)
								if e != nil {
									b.Fatal(e)
								}
								output = len(value)
								conversionBenchmarkSink = value
							case "ClaudeCloaking":
								d := ojson.NewDoc(translated)
								_, _, e := applyCloakingInternalDoc(ctx, cfg, auth, d, "sk-ant-api03-fixture-unused", false, false, false)
								if e != nil {
									b.Fatal(e)
								}
								value := d.Bytes()
								output = len(value)
								conversionBenchmarkSink = value
							case "ClaudeHeaders":
								r, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
								if e := applyClaudeHeadersWithNativeProfile(r, auth, "sk-ant-api03-fixture-unused", true, nil, translated, cfg, nil, false, false); e != nil {
									b.Fatal(e)
								}
								var wire bytes.Buffer
								_ = r.Header.Write(&wire)
								output = wire.Len()
								conversionBenchmarkSink = r.Header
							case "MetadataValidation":
								d := ojson.NewDoc(translated)
								if _, e := helps.ApplyClaudeCredentialMetadataDoc(d, auth, "00000000-0000-4000-8000-000000000001"); e != nil {
									b.Fatal(e)
								}
								output = d.Len()
								conversionBenchmarkSink = d
							}
						}
						b.StopTimer()
						b.ReportMetric((benchmarkCPUSeconds()-cpu)*1e9/float64(b.N), "cpu-ns/op")
						b.ReportMetric((benchmarkGCSeconds()-gc)*1e9/float64(b.N), "gc-cpu-ns/op")
						b.ReportMetric(float64(len(payload)), "input-B/op")
						b.ReportMetric(float64(output), "output-B/op")
					})
				}
			}
		}
	}
}
