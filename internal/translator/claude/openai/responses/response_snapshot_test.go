package responses

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

func TestResponseMetadataSnapshotPreservesEchoAndToolIdentities(t *testing.T) {
	request := []byte(`{"model":"fixture","instructions":"keep instructions","metadata":{"a":"value"},"reasoning":{"effort":"high"},"tools":[{"type":"function","name":"a.b","parameters":{"type":"object"}}],"input":[{"role":"user","content":"` + strings.Repeat("DOCUMENT_BYTES", 100000) + `"},{"type":"function_call","name":"a-b","arguments":"` + strings.Repeat("ARGUMENT_BYTES", 10000) + `"},{"type":"additional_tools","tools":[{"type":"function","name":"loaded","parameters":{"type":"object"}}]},{"type":"tool_search_output","tools":[{"type":"function","name":"discovered","parameters":{"type":"object"}}]}]}`)
	root := util.ParseGJSONBytesNoCopy(request)
	for _, search := range []bool{false, true} {
		wantEcho := extractResponsesRequestEcho(root)
		wantNames := buildClaudeToolNames(root, search)
		snapshot := snapshotResponsesRequestMetadata(request)
		if len(snapshot) > 2000 || bytes.Contains(snapshot, []byte("DOCUMENT_BYTES")) || bytes.Contains(snapshot, []byte("ARGUMENT_BYTES")) {
			t.Fatal("response state retained unrelated history")
		}
		state := newClaudeToResponsesState(request, search)
		if !reflect.DeepEqual(state.RequestEcho, wantEcho) || !reflect.DeepEqual(state.ToolNames, wantNames) {
			t.Fatal("snapshot changed response echo or tool-name collision resolution")
		}
	}
	state := newClaudeToResponsesState(request, true)
	for i := range request {
		request[i] = ' '
	}
	if state.RequestEcho.instructions != "keep instructions" || state.ToolNames.claudeName("loaded") == "" {
		t.Fatal("response state aliases caller-owned bytes")
	}
}
