package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexCompactPreservesOriginalUsageForCPA(t *testing.T) {
	const raw = `{"input_tokens":80,"output_tokens":27,"input_tokens_details":{"cached_tokens":70},"output_tokens_details":{"reasoning_tokens":20},"total_tokens":107}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/responses/compact" {
			t.Error("compact path changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmp_fixture","object":"response.compaction","usage":`+raw+`}`)
	}))
	defer server.Close()
	capture := &codexResponseModelUsageCapture{alias: t.Name(), records: make(chan coreusage.Record, 4)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { coreusage.RegisterNamedPlugin(t.Name(), codexResponseModelNoopUsagePlugin{}) })
	ctx := coreusage.WithRequestedModelAlias(coreusage.WithRawUsageCapture(context.Background()), t.Name())
	e := NewCodexExecutor(&config.Config{})
	_, err := e.Execute(ctx, codexOAuthTestAuth(server.URL), coreexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","input":[{"type":"message","role":"user","content":"fixture"}]}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Alt: "responses/compact"})
	if err != nil {
		t.Fatal(err)
	}
	record := capture.await(t)
	if record.Failed || record.RawUsageFormat != "responses" || string(record.RawUsage) != raw {
		t.Fatalf("actual compact executor omitted or changed original usage: format=%s raw=%s", record.RawUsageFormat, record.RawUsage)
	}
}
