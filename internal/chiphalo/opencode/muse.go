package opencode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/compat"
)

const MuseFormatError = "Upstream Muse returned an invalid, conflicting or undeclared tool call; this response cannot be completed safely."

const nativeCodeModeInstructions = `CODE-MODE TOOL ROUTING: Invoke only tools explicitly declared in the request's tools array. Every API documented inside functions.exec, under tools.*, must run as JavaScript inside functions__exec (client name functions.exec). This applies to file reads, shell commands, image viewing, MCP APIs and all other documented nested APIs. Their documentation does not declare additional top-level tools. Put the JavaScript into the single value string. Never emit a documented nested API such as exec_command as a separate top-level call. Example arguments: {"value":"const r = await tools.exec_command({cmd: 'echo hello'}); text(r.output);"}. Emit text results with text(...) and image results with image(...); returning a value or leaving a last expression does not display it. console.log and print are unavailable. Do not throw an error to display a successful result. Keep all other explicitly declared tools available.`

const nativeCodeModeImageInstructions = `CODE-MODE IMAGE OUTPUT: The documented view_image API must be called inside functions__exec, not as a top-level tool. Forward its returned image to the model: const result = await tools.view_image({path: 'absolute/image/path.png'}); image(result.image_url);. Printing a caption such as "viewing" or "ok" does not display the image. Only use APIs actually documented in the current functions.exec declaration.`

var museName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// MuseReferences retains only successful provider item identities, not content.
// A downstream caller, session or credential change cannot borrow an identity.
type MuseReferences struct {
	mu  sync.Mutex
	ids map[string]time.Time
	now func() time.Time
}

func NewMuseReferences() *MuseReferences {
	return &MuseReferences{ids: make(map[string]time.Time), now: time.Now}
}

func (s *MuseReferences) known(scope, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now().Before(s.ids[scope+"\x00"+id])
}

func (s *MuseReferences) remember(scope string, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, expires := range s.ids {
		if !now.Before(expires) {
			delete(s.ids, k)
		}
	}
	for _, id := range ids {
		if id == "" || len(id) > 1024 {
			continue
		}
		if len(s.ids) >= 4096 {
			var oldest string
			var expires time.Time
			for k, v := range s.ids {
				if expires.IsZero() || v.Before(expires) {
					oldest, expires = k, v
				}
			}
			delete(s.ids, oldest)
		}
		s.ids[scope+"\x00"+id] = now.Add(15 * time.Minute)
	}
}

type museTool struct {
	name, namespace string
	custom          bool
	inputField      string
}

func museWireToolName(name, namespace string) string {
	internal := name
	if namespace != "" {
		internal = namespace + "__" + name
	}
	if !museName.MatchString(internal) {
		sum := sha256.Sum256([]byte(namespace + "\x00" + name))
		internal = "cpa_tool_" + hex.EncodeToString(sum[:16])
	}
	return internal
}

type museCall struct {
	tool        museTool
	arguments   string
	done        bool
	callID      string
	fingerprint string
}

// MuseBridge changes only the native tool envelopes. General protocol
// translation stays in the upstream SDK; this adapter never evaluates tools.
type MuseBridge struct {
	native     bool
	tools      map[string]museTool
	names      map[string]string
	final      map[string]json.RawMessage
	calls      map[string]*museCall
	refs       *MuseReferences
	scope      string
	sequence   int64
	buffered   int
	pendingIDs []string
}

// PrepareMuse runs BEFORE the final payload rules. BindFinal subsequently reads
// the actual final declarations and validates them without rewriting anything.
func PrepareMuse(raw []byte, refs *MuseReferences, scope string) ([]byte, *MuseBridge, error) {
	return prepareNativeTools(raw, refs, scope, false)
}

// PrepareNativeResponses shares the reversible namespace/custom-tool envelope
// adapter, without Muse's provider-specific replay and tool-choice restrictions.
func PrepareNativeResponses(raw []byte, refs *MuseReferences, scope string) ([]byte, *MuseBridge, error) {
	return prepareNativeTools(raw, refs, scope, true)
}

func prepareNativeTools(raw []byte, refs *MuseReferences, scope string, native bool) ([]byte, *MuseBridge, error) {
	if refs == nil {
		refs = NewMuseReferences()
	}
	b := &MuseBridge{native: native, tools: map[string]museTool{}, names: map[string]string{}, final: map[string]json.RawMessage{}, calls: map[string]*museCall{}, refs: refs, scope: scope}
	// Admission may already have normalized the public model. Executors receive
	// the routed model, so use a temporary public prefix only for this shared gate.
	model := ojson.GetBytes(raw, "model").String()
	public, err := ojson.SetBytes(raw, "model", "opencode/"+strings.TrimPrefix(model, "opencode/"))
	if err != nil {
		return nil, nil, err
	}
	public, err = compat.NormalizeOpenCodeTools(public)
	if err != nil {
		return nil, nil, err
	}
	raw, _ = ojson.SetBytes(public, "model", model)
	if prev := ojson.GetBytes(raw, "previous_response_id"); !native && prev.Exists() && prev.Type != ojson.Null && prev.String() != "" {
		return nil, nil, fmt.Errorf("Muse continuation requires the CPA scoped full-context replay; resend full context")
	}
	flattened := make([]json.RawMessage, 0)
	codeMode := false
	codeModeImages := false
	var flatten func(ojson.Result, string, int) error
	flatten = func(list ojson.Result, ns string, depth int) error {
		if !list.IsArray() || depth > 16 {
			return fmt.Errorf("invalid Muse tool declarations")
		}
		for _, tool := range list.Array() {
			kind := tool.Get("type").String()
			if kind == "namespace" {
				if err := flatten(tool.Get("tools"), tool.Get("name").String(), depth+1); err != nil {
					return err
				}
				continue
			}
			if kind != "function" && kind != "custom" {
				if native {
					flattened = append(flattened, json.RawMessage(tool.Raw))
					continue
				}
				return fmt.Errorf("Muse accepts only function/custom tool declarations")
			}
			name := tool.Get("name").String()
			if name == "" {
				return fmt.Errorf("Muse tool name is required")
			}
			toolNS := ns
			if value := tool.Get("namespace").String(); value != "" {
				toolNS = value
			}
			key := toolNS + "\x00" + name
			if _, exists := b.names[key]; exists {
				return fmt.Errorf("duplicate Muse tool declaration")
			}
			internal := museWireToolName(name, toolNS)
			if _, exists := b.tools[internal]; exists {
				return fmt.Errorf("colliding Muse tool names")
			}
			inputField := "input"
			if native {
				inputField = "value"
			}
			b.names[key], b.tools[internal] = internal, museTool{name: name, namespace: toolNS, custom: kind == "custom", inputField: inputField}
			out := []byte(tool.Raw)
			out, _ = ojson.SetBytes(out, "name", internal)
			out, _ = ojson.DeleteBytes(out, "namespace")
			description := tool.Get("description").String()
			if native && toolNS != "" {
				description = "Tool " + toolNS + "." + name + ". " + description
				out, _ = ojson.SetBytes(out, "description", description)
			}
			if kind == "custom" {
				if grammar := tool.Get("format.definition").String(); tool.Get("format.type").String() == "grammar" && grammar != "" {
					description += "\nRaw input must follow this grammar:\n" + grammar
				}
				if native && toolNS == "functions" && name == "exec" && strings.Contains(description, "tools.exec_command") && strings.Contains(description, "text(") {
					codeMode = true
					codeModeImages = strings.Contains(description, "### `view_image`") && strings.Contains(description, "image_url")
					// Nested code-mode APIs are documentation, not callable wire tools.
					description = "CODE-MODE CALL CONTRACT: Call this declared tool using the single value string containing raw JavaScript. APIs documented as tools.* (including tools.exec_command) must be invoked inside that JavaScript; never emit them as separate top-level tool calls. Emit results explicitly with text(...). The last expression is not displayed. console.log and print are unavailable; do not throw errors to display successful results. Example value: const r = await tools.exec_command({cmd: 'echo hello'}); text(r.output);\n\n" + description
				}
				out, _ = json.Marshal(map[string]any{"type": "function", "name": internal, "description": description, "parameters": map[string]any{"type": "object", "properties": map[string]any{inputField: map[string]any{"type": "string"}}, "required": []string{inputField}, "additionalProperties": false}})
			}
			flattened = append(flattened, out)
			if len(flattened) > 512 {
				return fmt.Errorf("Muse tool declaration limit")
			}
		}
		return nil
	}
	if tools := ojson.GetBytes(raw, "tools"); tools.Exists() {
		if err := flatten(tools, "", 0); err != nil {
			return nil, nil, err
		}
		raw, _ = ojson.SetBytes(raw, "tools", flattened)
	}
	if codeMode {
		instructions := ojson.GetBytes(raw, "instructions").String()
		guidance := nativeCodeModeInstructions
		if codeModeImages {
			guidance += "\n\n" + nativeCodeModeImageInstructions
		}
		if !strings.Contains(instructions, guidance) {
			if instructions != "" {
				instructions = "\n\n" + instructions
			}
			raw, _ = ojson.SetBytes(raw, "instructions", guidance+instructions)
		}
	}
	choice := ojson.GetBytes(raw, "tool_choice")
	if choice.Exists() && native && choice.IsObject() && (choice.Get("type").String() == "function" || choice.Get("type").String() == "custom") {
		name := b.names[choice.Get("namespace").String()+"\x00"+choice.Get("name").String()]
		// Native clients may qualify a named tool as namespace.name instead of
		// using separate fields. Resolve only against the declared tool map;
		// an exact unqualified declaration always takes precedence.
		if name == "" && choice.Get("namespace").String() == "" {
			qualified := choice.Get("name").String()
			if dot := strings.LastIndexByte(qualified, '.'); dot > 0 {
				name = b.names[qualified[:dot]+"\x00"+qualified[dot+1:]]
			}
		}
		if name == "" {
			return nil, nil, fmt.Errorf("named Responses tool is not declared")
		}
		raw, _ = ojson.SetBytes(raw, "tool_choice.name", name)
		raw, _ = ojson.DeleteBytes(raw, "tool_choice.namespace")
		raw, _ = ojson.SetBytes(raw, "tool_choice.type", "function")
	}
	if choice.Exists() && !native {
		if choice.Type != ojson.String {
			return nil, nil, fmt.Errorf("Muse does not support required or named tool selection")
		}
		switch choice.String() {
		case "auto":
		case "none":
			raw, _ = ojson.DeleteBytes(raw, "tools")
			raw, _ = ojson.SetBytes(raw, "tool_choice", "auto")
		default:
			return nil, nil, fmt.Errorf("Muse does not support required or named tool selection")
		}
	}
	for i, item := range ojson.GetBytes(raw, "input").Array() {
		path := fmt.Sprintf("input.%d", i)
		switch item.Get("type").String() {
		case "reasoning":
			if native {
				continue
			}
			id := item.Get("id").String()
			if !refs.known(scope, id) {
				if item.Get("encrypted_content").String() != "" {
					return nil, nil, fmt.Errorf("foreign encrypted reasoning cannot be replayed to Muse; replay portable history or start a new conversation")
				}
				raw, _ = ojson.DeleteBytes(raw, path+".id")
			}
		case "function_call", "custom_tool_call":
			name, ns := item.Get("name").String(), item.Get("namespace").String()
			internal := b.names[ns+"\x00"+name]
			if internal == "" && ns == "" {
				for key, info := range b.tools {
					if info.name == name {
						if internal != "" {
							return nil, nil, fmt.Errorf("ambiguous Muse history tool")
						}
						internal = key
					}
				}
			}
			inputField := b.tools[internal].inputField
			if internal == "" {
				if !native {
					return nil, nil, fmt.Errorf("Muse history tool is not declared in this request")
				}
				if name == "" {
					return nil, nil, fmt.Errorf("Responses history tool name is required")
				}
				// Completed history can reference tools removed from the current
				// declaration set. Keep replay identities without authorizing new
				// calls to those tools through the response validation maps.
				internal = museWireToolName(name, ns)
				inputField = "value"
			}
			raw, _ = ojson.SetBytes(raw, path+".name", internal)
			raw, _ = ojson.DeleteBytes(raw, path+".namespace")
			raw, _ = ojson.DeleteBytes(raw, path+".id")
			if item.Get("type").String() == "custom_tool_call" {
				if item.Get("input").Type != ojson.String {
					return nil, nil, fmt.Errorf("invalid custom tool history input")
				}
				args, _ := json.Marshal(map[string]string{inputField: item.Get("input").String()})
				raw, _ = ojson.SetBytes(raw, path+".type", "function_call")
				raw, _ = ojson.SetBytes(raw, path+".arguments", string(args))
				raw, _ = ojson.DeleteBytes(raw, path+".input")
			}
		case "custom_tool_call_output":
			raw, _ = ojson.SetBytes(raw, path+".type", "function_call_output")
		}
	}
	return raw, b, nil
}

func (b *MuseBridge) BindFinal(raw []byte) error {
	if tools := ojson.GetBytes(raw, "tools"); tools.Exists() && !tools.IsArray() {
		return fmt.Errorf("final Muse tools must be an array")
	}
	choice := ojson.GetBytes(raw, "tool_choice")
	if !b.native && choice.Exists() && (choice.Type != ojson.String || choice.String() != "auto") {
		return fmt.Errorf("final Muse payload requires auto tool_choice; user payload rules were not overridden")
	}
	for _, tool := range ojson.GetBytes(raw, "tools").Array() {
		if b.native && tool.Get("type").String() == "custom" {
			return fmt.Errorf("native custom tools require the prepared function envelope")
		}
		if b.native && tool.Get("type").String() != "function" {
			continue
		}
		name := tool.Get("name").String()
		if tool.Get("type").String() != "function" || !museName.MatchString(name) || !tool.Get("parameters").IsObject() {
			return fmt.Errorf("final Muse tool declaration is invalid")
		}
		if _, exists := b.final[name]; exists {
			return fmt.Errorf("duplicate final Muse tool")
		}
		b.final[name] = json.RawMessage(tool.Get("parameters").Raw)
		if _, exists := b.tools[name]; !exists {
			b.tools[name] = museTool{name: name}
		}
	}
	for _, item := range ojson.GetBytes(raw, "input").Array() {
		if item.Get("type").String() == "additional_tools" {
			return fmt.Errorf("final Muse payload contains unsupported additional_tools")
		}
		if item.Get("type").String() == "reasoning" && item.Get("id").String() != "" && !b.refs.known(b.scope, item.Get("id").String()) {
			if b.native {
				continue
			}
			return fmt.Errorf("final Muse payload contains an unowned reasoning reference")
		}
	}
	return nil
}

func (b *MuseBridge) restoreItem(raw []byte, partial bool) ([]byte, error) {
	if b.native && ojson.GetBytes(raw, "type").String() == "custom_tool_call" {
		return nil, fmt.Errorf("upstream returned an undeclared native custom call type")
	}
	if ojson.GetBytes(raw, "type").String() != "function_call" {
		return raw, nil
	}
	name := ojson.GetBytes(raw, "name").String()
	info, ok := b.tools[name]
	if !ok || b.final[name] == nil {
		return nil, fmt.Errorf("undeclared Muse tool call")
	}
	id := ojson.GetBytes(raw, "id").String()
	if id == "" || len(id) > 1024 {
		return nil, fmt.Errorf("invalid Muse call identity")
	}
	if existing := b.calls[id]; existing != nil && existing.tool != info {
		return nil, fmt.Errorf("conflicting Muse call identity")
	}
	if b.calls[id] == nil {
		if len(b.calls) >= MaxCalls {
			return nil, fmt.Errorf("Muse tool call limit")
		}
		b.calls[id] = &museCall{tool: info}
	}
	callID := ojson.GetBytes(raw, "call_id").String()
	if callID != "" {
		if len(callID) > 1024 || (b.calls[id].callID != "" && b.calls[id].callID != callID) {
			return nil, fmt.Errorf("conflicting Muse call ID")
		}
		b.calls[id].callID = callID
	}
	if !partial && b.calls[id].callID == "" {
		return nil, fmt.Errorf("Muse call_id is missing")
	}
	args := ojson.GetBytes(raw, "arguments").String()
	if !partial {
		if len(args) > MaxChars {
			return nil, fmt.Errorf("Muse argument limit")
		}
		value, err := strictJSON(args)
		if err != nil {
			return nil, err
		}
		schema, err := compileSchema(b.final[name])
		if err != nil || schema.Validate(value) != nil {
			return nil, fmt.Errorf("Muse arguments do not match final declaration")
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(canonical)
		fingerprint := hex.EncodeToString(sum[:])
		if b.calls[id].fingerprint != "" && b.calls[id].fingerprint != fingerprint {
			return nil, fmt.Errorf("Muse terminal call arguments conflict")
		}
		b.calls[id].fingerprint = fingerprint
	}
	raw, _ = ojson.SetBytes(raw, "name", info.name)
	if info.namespace != "" {
		raw, _ = ojson.SetBytes(raw, "namespace", info.namespace)
	}
	if info.custom {
		raw, _ = ojson.SetBytes(raw, "type", "custom_tool_call")
		raw, _ = ojson.SetBytes(raw, "id", "ctc_"+strings.TrimPrefix(id, "fc_"))
		input := ""
		if !partial {
			value, err := strictJSON(args)
			if err != nil {
				return nil, err
			}
			object, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid Muse custom tool envelope")
			}
			input, ok = object[info.inputField].(string)
			if !ok || len(object) != 1 {
				return nil, fmt.Errorf("invalid Muse custom tool input")
			}
		}
		raw, _ = ojson.DeleteBytes(raw, "arguments")
		raw, _ = ojson.SetBytes(raw, "input", input)
	}
	return raw, nil
}

func (b *MuseBridge) Response(raw []byte) ([]byte, error) {
	var err error
	for i, item := range ojson.GetBytes(raw, "output").Array() {
		out, errItem := b.restoreItem([]byte(item.Raw), false)
		if errItem != nil {
			return nil, errItem
		}
		raw, err = ojson.SetRawBytes(raw, fmt.Sprintf("output.%d", i), out)
		if err != nil {
			return nil, err
		}
		if item.Get("type").String() == "reasoning" {
			b.pendingIDs = append(b.pendingIDs, item.Get("id").String())
		}
	}
	if ojson.GetBytes(raw, "status").String() == "completed" {
		b.refs.remember(b.scope, b.pendingIDs)
	}
	return raw, nil
}

func (b *MuseBridge) Event(raw []byte) ([][]byte, error) {
	if !ojson.ValidBytes(raw) {
		return nil, fmt.Errorf("invalid Muse event")
	}
	kind := ojson.GetBytes(raw, "type").String()
	var err error
	if kind == "response.output_item.added" || kind == "response.output_item.done" {
		item := ojson.GetBytes(raw, "item")
		out, errItem := b.restoreItem([]byte(item.Raw), kind == "response.output_item.added")
		if errItem != nil {
			return nil, errItem
		}
		raw, err = ojson.SetRawBytes(raw, "item", out)
		if kind == "response.output_item.done" && item.Get("type").String() == "reasoning" {
			b.pendingIDs = append(b.pendingIDs, item.Get("id").String())
		}
	}
	id := ojson.GetBytes(raw, "item_id").String()
	if b.native && strings.HasPrefix(kind, "response.custom_tool_call_input.") {
		return nil, fmt.Errorf("upstream emitted a custom event for a function envelope")
	}
	if call := b.calls[id]; call != nil && strings.HasPrefix(kind, "response.function_call_arguments.") {
		if call.done {
			return nil, fmt.Errorf("Muse call emitted arguments after completion")
		}
		if kind == "response.function_call_arguments.done" && !call.tool.custom {
			name := b.names[call.tool.namespace+"\x00"+call.tool.name]
			if name == "" {
				name = call.tool.name
			}
			item, _ := json.Marshal(map[string]any{"type": "function_call", "id": id, "call_id": call.callID, "name": name, "arguments": ojson.GetBytes(raw, "arguments").String()})
			if _, err := b.restoreItem(item, false); err != nil {
				return nil, err
			}
			call.done = true
		}
		if call.tool.custom {
			if kind == "response.function_call_arguments.delta" {
				delta := ojson.GetBytes(raw, "delta").String()
				b.buffered += len(delta)
				if b.buffered > 4*MaxChars || len(call.arguments)+len(delta) > MaxChars {
					return nil, fmt.Errorf("Muse argument buffer limit")
				}
				call.arguments += delta
				return nil, nil
			}
			args := ojson.GetBytes(raw, "arguments").String()
			if args == "" {
				args = call.arguments
			}
			item, _ := json.Marshal(map[string]any{"type": "function_call", "id": id, "call_id": call.callID, "name": b.names[call.tool.namespace+"\x00"+call.tool.name], "arguments": args})
			out, errItem := b.restoreItem(item, false)
			if errItem != nil {
				return nil, errItem
			}
			input := ojson.GetBytes(out, "input").String()
			raw, _ = ojson.SetBytes(raw, "item_id", ojson.GetBytes(out, "id").String())
			raw, _ = ojson.DeleteBytes(raw, "arguments")
			raw, _ = ojson.SetBytes(raw, "type", "response.custom_tool_call_input.done")
			raw, _ = ojson.SetBytes(raw, "input", input)
			delta, _ := ojson.SetBytes(raw, "type", "response.custom_tool_call_input.delta")
			delta, _ = ojson.DeleteBytes(delta, "input")
			delta, _ = ojson.SetBytes(delta, "delta", input)
			call.done = true
			return b.number([][]byte{delta, raw}), nil
		}
	} else if strings.HasPrefix(kind, "response.function_call_arguments.") {
		return nil, fmt.Errorf("Muse arguments lack a declared call")
	}
	if response := ojson.GetBytes(raw, "response"); response.IsObject() {
		out, errResponse := b.Response([]byte(response.Raw))
		if errResponse != nil {
			return nil, errResponse
		}
		raw, err = ojson.SetRawBytes(raw, "response", out)
	}
	if err != nil {
		return nil, err
	}
	return b.number([][]byte{raw}), nil
}

func (b *MuseBridge) number(events [][]byte) [][]byte {
	for i, raw := range events {
		if ojson.GetBytes(raw, "sequence_number").Exists() {
			events[i], _ = ojson.SetBytes(raw, "sequence_number", b.sequence)
			b.sequence++
		}
	}
	return events
}
