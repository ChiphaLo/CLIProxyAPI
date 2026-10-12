package pluginhost

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestGeminiPluginAdapterCapturesRawUsageBeforeTranslation(t *testing.T) {
	const raw = `{"promptTokenCount":100,"candidatesTokenCount":20,"thoughtsTokenCount":30,"cachedContentTokenCount":80}`
	const payload = `{"modelVersion":"gemini-plugin-fixture","candidates":[{"content":{"role":"model","parts":[{"text":"fixture"}]},"finishReason":"STOP"}],"usageMetadata":` + raw + `}`
	for _, stream := range []bool{false, true} {
		name := "gemini-plugin-raw-execute"
		if stream {
			name = "gemini-plugin-raw-stream"
		}
		t.Run(name, func(t *testing.T) {
			plugin := newTestUsageCapturePlugin("gemini-cli")
			registerTestUsagePlugin(t, name, plugin)
			record := normalizeTestCapabilityRecord(capabilityRecord{id: name})
			host := newHostWithRecords(record)
			exec := &fakeExecutor{identifier: "gemini-cli",
				execute: func(context.Context, pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
					return pluginapi.ExecutorResponse{Payload: []byte(payload)}, nil
				},
				executeStream: func(context.Context, pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
					chunks := make(chan pluginapi.ExecutorStreamChunk, 4)
					chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte("data: {\"modelVersion\":\"gemini-plugin-fixture\",\"usageMetadata\":{\"promptTokenCount\":100}}\n\n")}
					line := "data: " + payload + "\n\n"
					chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte(line[:37])}
					chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte(line[37:])}
					close(chunks)
					return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
				},
			}
			adapter := newExecutorAdapterForRecordForTest(host, record, exec, []sdktranslator.Format{sdktranslator.FormatGemini}, []sdktranslator.Format{sdktranslator.FormatGemini})
			adapter.provider = "gemini-cli"
			ctx := coreusage.WithRawUsageCapture(context.Background())
			auth := &coreauth.Auth{ID: "gemini-plugin-account", Provider: "gemini-cli"}
			req := coreexecutor.Request{Model: "gemini-plugin-fixture", Payload: []byte(`{"contents":[{"parts":[{"text":"fixture"}]}]}`)}
			opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini}
			if stream {
				result, err := adapter.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else {
				if _, err := adapter.Execute(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
			}
			usage := plugin.waitRecord(t, 5*time.Second)
			if usage.RawUsageFormat != "gemini" || string(usage.RawUsage) != raw || usage.ResponseModel != "gemini-plugin-fixture" {
				t.Fatalf("plugin raw receipt metadata lost: %+v", usage)
			}
		})
	}
}
