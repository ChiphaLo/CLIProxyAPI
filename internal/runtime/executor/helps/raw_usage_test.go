package helps

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestRawUsageCaptureIsOptIn(t *testing.T) {
	payload := []byte(`{"object":"chat.completion","model":"reported","usage":{"prompt_tokens":12,"completion_tokens":7}}`)
	for _, enabled := range []bool{false, true} {
		ctx := context.Background()
		if enabled {
			ctx = usage.WithRawUsageCapture(ctx)
		}
		r := NewUsageReporter(ctx, "fixture", "requested", nil)
		r.ObserveResponseModel(payload)
		record := r.buildRecord(usage.Detail{InputTokens: 9999}, false)
		if record.ResponseModel != "reported" || record.Detail.InputTokens != 9999 {
			t.Fatal("existing metadata/accounting changed")
		}
		if enabled {
			if string(record.RawUsage) != `{"prompt_tokens":12,"completion_tokens":7}` || record.RawUsageFormat != "chat" {
				t.Fatalf("raw usage not preserved: %s", record.RawUsage)
			}
		} else if len(record.RawUsage) > 0 {
			t.Fatal("raw usage retained without opting in")
		}
	}
}

func TestRawResponsesCompactionAndTerminalRootUsage(t *testing.T) {
	for _, identity := range []string{`"object":"response.compaction"`, `"type":"response.completed"`, `"type":"response.done"`, `"type":"response.incomplete"`} {
		r := NewUsageReporter(usage.WithRawUsageCapture(context.Background()), "codex", "fixture", nil)
		raw := `{"input_tokens":51,"output_tokens":17,"input_tokens_details":{"cached_tokens":40},"output_tokens_details":{"reasoning_tokens":12}}`
		r.ObserveResponseModel([]byte(`{` + identity + `,"usage":` + raw + `}`))
		if got, format := r.rawUsageSnapshot(); string(got) != raw || format != "responses" {
			t.Fatalf("known Responses envelope lost raw usage: %s / %s", got, format)
		}
	}
}

func TestStreamObserverKeepsLateUsageAfterFinalModelIdentity(t *testing.T) {
	r := NewUsageReporter(usage.WithRawUsageCapture(context.Background()), "fixture", "requested", nil)
	o := NewStreamResponseModelObserver(r)
	o.Feed([]byte("data: {\"type\":\"response.completed\",\"response\":{\"model\":\"served\"}}\n\n"))
	late := "data: {\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":70,\"completion_tokens\":8}}\n\n"
	for _, chunk := range []string{late[:31], late[31:65], late[65:]} {
		o.Feed([]byte(chunk))
	}
	o.Finish()
	if raw, format := r.rawUsageSnapshot(); string(raw) != `{"prompt_tokens":70,"completion_tokens":8}` || format != "chat" || r.ResponseModel() != "served" {
		t.Fatalf("late usage lost or final model mutated: %s / %s", raw, format)
	}
}

func TestRawUsageAfterModelIdentityFinal(t *testing.T) {
	r := NewUsageReporter(usage.WithRawUsageCapture(context.Background()), "fixture", "requested", nil)
	r.ObserveResponseModel([]byte(`{"type":"response.completed","response":{"model":"reported","usage":null}}`))
	r.ObserveResponseModel([]byte(`data: {"choices":[],"usage":{"prompt_tokens":1001,"completion_tokens":137,"completion_tokens_details":{"reasoning_tokens":100}}}`))
	record := r.buildRecord(usage.Detail{}, false)
	if record.ResponseModel != "reported" || record.RawUsageFormat != "chat" || !strings.Contains(string(record.RawUsage), `"reasoning_tokens":100`) {
		t.Fatal("late usage frame lost")
	}
	// Records own their bytes; subsequent frames cannot mutate published usage.
	r.ObserveResponseModel([]byte(`{"usage":{"prompt_tokens":2}}`))
	if !strings.Contains(string(record.RawUsage), `"prompt_tokens":1001`) {
		t.Fatal("published record aliased reporter buffer")
	}
}

func TestRawUsageRejectsUnknownFormatsAndBounds(t *testing.T) {
	r := NewUsageReporter(usage.WithRawUsageCapture(context.Background()), "fixture", "requested", nil)
	for _, payload := range []string{
		`{"type":"message","usage":{"input_tokens":55,"output_tokens":3}}`,
		`{"usageMetadata":null}`,
		`data: [DONE]`,
		`{"object":"response","usage":{}}`,
		`{"type":"response.completed","response":{"usage":{"service_tier":"default"}}}`,
		`{"object":"response","usage":{"extra":"` + strings.Repeat("x", maxRawUsageBytes) + `"}}`,
	} {
		r.ObserveResponseModel([]byte(payload))
	}
	if data, format := r.rawUsageSnapshot(); len(data) != 0 || format != "" {
		t.Fatal("unknown/unbounded usage must not be guessed")
	}
	r.ObserveResponseModel([]byte(`{"type":"response.completed","response":{"model":"served","usage":{"input_tokens":55,"output_tokens":3}}}`))
	record := r.buildRecord(usage.Detail{}, false)
	if string(record.RawUsage) != `{"input_tokens":55,"output_tokens":3}` || record.RawUsageFormat != "responses" {
		t.Fatal("Responses usage not retained")
	}
	sideRecord := r.buildRecordForModel("side-image-model", usage.Detail{}, false, usage.Failure{})
	if len(sideRecord.RawUsage) != 0 {
		t.Fatal("main response usage copied to side model")
	}
}

func TestRawUsagePreservesLegacyDuplicateKeySelection(t *testing.T) {
	r := NewUsageReporter(usage.WithRawUsageCapture(context.Background()), "fixture", "requested", nil)
	r.ObserveResponseModel([]byte(`{"object":"chat.completion","usage":{"prompt_tokens":100},"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	if raw, format := r.rawUsageSnapshot(); string(raw) != `{"prompt_tokens":3,"completion_tokens":4}` || format != "chat" {
		t.Fatal("duplicate top-level usage changed legacy accounting")
	}
	r.ObserveResponseModel([]byte(`{"response":{"usage":{"input_tokens":100}},"response":{"usage":{"input_tokens":3},"usage":{"input_tokens":5,"output_tokens":6}}}`))
	if raw, format := r.rawUsageSnapshot(); string(raw) != `{"input_tokens":5,"output_tokens":6}` || format != "responses" {
		t.Fatal("duplicate nested usage changed legacy accounting")
	}
}

func TestRawGeminiUsageSnapshotsFinalFrameAndEnvelopes(t *testing.T) {
	for _, envelope := range []string{"usageMetadata", "response", "usage_metadata"} {
		t.Run(envelope, func(t *testing.T) {
			r := NewUsageReporter(usage.WithRawUsageCapture(context.Background()), "gemini-cli", "requested", nil)
			r.ObserveResponseModel([]byte(`data: {"modelVersion":"gemini-fixture","usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":1}}`))
			raw := `{"promptTokenCount":100,"candidatesTokenCount":20,"thoughtsTokenCount":30,"cachedContentTokenCount":80}`
			payload := `{"` + envelope + `":` + raw + `}`
			if envelope == "response" {
				payload = `{"response":{"modelVersion":"gemini-fixture","usageMetadata":` + raw + `}}`
			}
			r.ObserveResponseModel([]byte("data: " + payload))
			record := r.buildRecord(usage.Detail{InputTokens: 9999}, false)
			if string(record.RawUsage) != raw || record.RawUsageFormat != "gemini" {
				t.Fatalf("raw Gemini usage lost: %+v", record)
			}
			if strings.Contains(string(record.RawUsage), "modelVersion") || strings.Contains(string(record.RawUsage), "candidates\"") {
				t.Fatal("response content retained")
			}
		})
	}
}
