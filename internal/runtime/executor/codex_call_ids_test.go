package executor

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexRequestBuildersRepair78CharacterCallIDs(t *testing.T) {
	long := "call_" + strings.Repeat("d", 73)
	body := []byte(fmt.Sprintf(`{"model":"gpt-6.1-sol","input":[{"type":"function_call","id":"fc_history","call_id":%q,"name":"probe","arguments":"{}"},{"type":"function_call_output","call_id":%q,"output":"HISTORY_OK"}]}`, long, long))
	check := func(raw []byte) {
		t.Helper()
		id := ojson.GetBytes(raw, "input.0.call_id").String()
		if id == "" || len(id) > 64 || id == long || id != ojson.GetBytes(raw, "input.1.call_id").String() {
			t.Fatalf("request has invalid or unmatched call IDs: %s", raw)
		}
		if ojson.GetBytes(raw, "input.1.output").String() != "HISTORY_OK" {
			t.Fatal("tool result was lost")
		}
	}
	finalized := false
	ctx := helps.WithPayloadFinalizer(context.Background(), func(raw []byte) []byte {
		check(raw)
		finalized = true
		return raw
	})
	e := NewCodexExecutor(&config.Config{})
	r, raw, err := e.cacheHelper(ctx, sdktranslator.FormatOpenAIResponse, "http://example.invalid/responses", coreexecutor.Request{Model: "gpt-6.1-sol", Payload: body}, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	check(raw)
	wire, err := io.ReadAll(r.Body)
	if err != nil || !finalized || string(wire) != string(raw) {
		t.Fatal("HTTP framing changed the finalized payload")
	}
	check(buildCodexWebsocketRequestBody(body))
}
