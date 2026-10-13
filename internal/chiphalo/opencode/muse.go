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

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/compat"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const MuseFormatError = "Upstream Muse returned an invalid, conflicting or undeclared tool call; this response cannot be completed safely."

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
	if refs == nil {
		refs = NewMuseReferences()
	}
	b := &MuseBridge{tools: map[string]museTool{}, names: map[string]string{}, final: map[string]json.RawMessage{}, calls: map[string]*museCall{}, refs: refs, scope: scope}
	// Admission may already have normalized the public model. Executors receive
	// the routed model, so use a temporary public prefix only for this shared gate.
	model := gjson.GetBytes(raw, "model").String()
	public, err := sjson.SetBytes(raw, "model", "opencode/"+strings.TrimPrefix(model, "opencode/"))
	if err != nil {
		return nil, nil, err
	}
	public, err = compat.NormalizeOpenCodeTools(public)
	if err != nil {
		return nil, nil, err
	}
	raw, _ = sjson.SetBytes(public, "model", model)
	if prev := gjson.GetBytes(raw, "previous_response_id"); prev.Exists() && prev.Type != gjson.Null && prev.String() != "" {
		return nil, nil, fmt.Errorf("Muse continuation requires the CPA scoped full-context replay; resend full context")
	}
	var flattened []json.RawMessage
	var flatten func(gjson.Result, string, int) error
	flatten = func(list gjson.Result, ns string, depth int) error {
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
			internal := name
			if toolNS != "" {
				internal = toolNS + "__" + name
			}
			if !museName.MatchString(internal) {
				sum := sha256.Sum256([]byte(key))
				internal = "cpa_tool_" + hex.EncodeToString(sum[:16])
			}
			if _, exists := b.tools[internal]; exists {
				return fmt.Errorf("colliding Muse tool names")
			}
			b.names[key], b.tools[internal] = internal, museTool{name, toolNS, kind == "custom"}
			out := []byte(tool.Raw)
			out, _ = sjson.SetBytes(out, "name", internal)
			out, _ = sjson.DeleteBytes(out, "namespace")
			if kind == "custom" {
				description := tool.Get("description").String()
				if grammar := tool.Get("format.definition").String(); tool.Get("format.type").String() == "grammar" && grammar != "" {
					description += "\nRaw input must follow this grammar:\n" + grammar
				}
				out, _ = json.Marshal(map[string]any{"type": "function", "name": internal, "description": description, "parameters": map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"}, "additionalProperties": false}})
			}
			flattened = append(flattened, out)
			if len(flattened) > 512 {
				return fmt.Errorf("Muse tool declaration limit")
			}
		}
		return nil
	}
	if tools := gjson.GetBytes(raw, "tools"); tools.Exists() {
		if err := flatten(tools, "", 0); err != nil {
			return nil, nil, err
		}
		raw, _ = sjson.SetBytes(raw, "tools", flattened)
	}
	choice := gjson.GetBytes(raw, "tool_choice")
	if choice.Exists() {
		if choice.Type != gjson.String {
			return nil, nil, fmt.Errorf("Muse does not support required or named tool selection")
		}
		switch choice.String() {
		case "auto":
		case "none":
			raw, _ = sjson.DeleteBytes(raw, "tools")
			raw, _ = sjson.SetBytes(raw, "tool_choice", "auto")
		default:
			return nil, nil, fmt.Errorf("Muse does not support required or named tool selection")
		}
	}
	for i, item := range gjson.GetBytes(raw, "input").Array() {
		path := fmt.Sprintf("input.%d", i)
		switch item.Get("type").String() {
		case "reasoning":
			id := item.Get("id").String()
			if !refs.known(scope, id) {
				if item.Get("encrypted_content").String() != "" {
					return nil, nil, fmt.Errorf("foreign encrypted reasoning cannot be replayed to Muse; replay portable history or start a new conversation")
				}
				raw, _ = sjson.DeleteBytes(raw, path+".id")
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
			if internal == "" {
				return nil, nil, fmt.Errorf("Muse history tool is not declared in this request")
			}
			raw, _ = sjson.SetBytes(raw, path+".name", internal)
			raw, _ = sjson.DeleteBytes(raw, path+".namespace")
			raw, _ = sjson.DeleteBytes(raw, path+".id")
			if item.Get("type").String() == "custom_tool_call" {
				if item.Get("input").Type != gjson.String {
					return nil, nil, fmt.Errorf("invalid custom tool history input")
				}
				args, _ := json.Marshal(map[string]string{"input": item.Get("input").String()})
				raw, _ = sjson.SetBytes(raw, path+".type", "function_call")
				raw, _ = sjson.SetBytes(raw, path+".arguments", string(args))
				raw, _ = sjson.DeleteBytes(raw, path+".input")
			}
		case "custom_tool_call_output":
			raw, _ = sjson.SetBytes(raw, path+".type", "function_call_output")
		}
	}
	return raw, b, nil
}

func (b *MuseBridge) BindFinal(raw []byte) error {
	if tools := gjson.GetBytes(raw, "tools"); tools.Exists() && !tools.IsArray() {
		return fmt.Errorf("final Muse tools must be an array")
	}
	choice := gjson.GetBytes(raw, "tool_choice")
	if choice.Exists() && (choice.Type != gjson.String || choice.String() != "auto") {
		return fmt.Errorf("final Muse payload requires auto tool_choice; user payload rules were not overridden")
	}
	for _, tool := range gjson.GetBytes(raw, "tools").Array() {
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
	for _, item := range gjson.GetBytes(raw, "input").Array() {
		if item.Get("type").String() == "additional_tools" {
			return fmt.Errorf("final Muse payload contains unsupported additional_tools")
		}
		if item.Get("type").String() == "reasoning" && item.Get("id").String() != "" && !b.refs.known(b.scope, item.Get("id").String()) {
			return fmt.Errorf("final Muse payload contains an unowned reasoning reference")
		}
	}
	return nil
}

func (b *MuseBridge) restoreItem(raw []byte, partial bool) ([]byte, error) {
	if gjson.GetBytes(raw, "type").String() != "function_call" {
		return raw, nil
	}
	name := gjson.GetBytes(raw, "name").String()
	info, ok := b.tools[name]
	if !ok || b.final[name] == nil {
		return nil, fmt.Errorf("undeclared Muse tool call")
	}
	id := gjson.GetBytes(raw, "id").String()
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
	callID := gjson.GetBytes(raw, "call_id").String()
	if callID != "" {
		if len(callID) > 1024 || (b.calls[id].callID != "" && b.calls[id].callID != callID) {
			return nil, fmt.Errorf("conflicting Muse call ID")
		}
		b.calls[id].callID = callID
	}
	if !partial && b.calls[id].callID == "" {
		return nil, fmt.Errorf("Muse call_id is missing")
	}
	args := gjson.GetBytes(raw, "arguments").String()
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
	raw, _ = sjson.SetBytes(raw, "name", info.name)
	if info.namespace != "" {
		raw, _ = sjson.SetBytes(raw, "namespace", info.namespace)
	}
	if info.custom {
		raw, _ = sjson.SetBytes(raw, "type", "custom_tool_call")
		raw, _ = sjson.SetBytes(raw, "id", "ctc_"+strings.TrimPrefix(id, "fc_"))
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
			input, ok = object["input"].(string)
			if !ok || len(object) != 1 {
				return nil, fmt.Errorf("invalid Muse custom tool input")
			}
		}
		raw, _ = sjson.DeleteBytes(raw, "arguments")
		raw, _ = sjson.SetBytes(raw, "input", input)
	}
	return raw, nil
}

func (b *MuseBridge) Response(raw []byte) ([]byte, error) {
	var err error
	for i, item := range gjson.GetBytes(raw, "output").Array() {
		out, errItem := b.restoreItem([]byte(item.Raw), false)
		if errItem != nil {
			return nil, errItem
		}
		raw, err = sjson.SetRawBytes(raw, fmt.Sprintf("output.%d", i), out)
		if err != nil {
			return nil, err
		}
		if item.Get("type").String() == "reasoning" {
			b.pendingIDs = append(b.pendingIDs, item.Get("id").String())
		}
	}
	if gjson.GetBytes(raw, "status").String() == "completed" {
		b.refs.remember(b.scope, b.pendingIDs)
	}
	return raw, nil
}

func (b *MuseBridge) Event(raw []byte) ([][]byte, error) {
	if !gjson.ValidBytes(raw) {
		return nil, fmt.Errorf("invalid Muse event")
	}
	kind := gjson.GetBytes(raw, "type").String()
	var err error
	if kind == "response.output_item.added" || kind == "response.output_item.done" {
		item := gjson.GetBytes(raw, "item")
		out, errItem := b.restoreItem([]byte(item.Raw), kind == "response.output_item.added")
		if errItem != nil {
			return nil, errItem
		}
		raw, err = sjson.SetRawBytes(raw, "item", out)
		if kind == "response.output_item.done" && item.Get("type").String() == "reasoning" {
			b.pendingIDs = append(b.pendingIDs, item.Get("id").String())
		}
	}
	id := gjson.GetBytes(raw, "item_id").String()
	if call := b.calls[id]; call != nil && strings.HasPrefix(kind, "response.function_call_arguments.") {
		if call.done {
			return nil, fmt.Errorf("Muse call emitted arguments after completion")
		}
		if kind == "response.function_call_arguments.done" && !call.tool.custom {
			name := b.names[call.tool.namespace+"\x00"+call.tool.name]
			if name == "" {
				name = call.tool.name
			}
			item, _ := json.Marshal(map[string]any{"type": "function_call", "id": id, "call_id": call.callID, "name": name, "arguments": gjson.GetBytes(raw, "arguments").String()})
			if _, err := b.restoreItem(item, false); err != nil {
				return nil, err
			}
			call.done = true
		}
		if call.tool.custom {
			if kind == "response.function_call_arguments.delta" {
				delta := gjson.GetBytes(raw, "delta").String()
				b.buffered += len(delta)
				if b.buffered > 4*MaxChars || len(call.arguments)+len(delta) > MaxChars {
					return nil, fmt.Errorf("Muse argument buffer limit")
				}
				call.arguments += delta
				return nil, nil
			}
			args := gjson.GetBytes(raw, "arguments").String()
			if args == "" {
				args = call.arguments
			}
			item, _ := json.Marshal(map[string]any{"type": "function_call", "id": id, "call_id": call.callID, "name": b.names[call.tool.namespace+"\x00"+call.tool.name], "arguments": args})
			out, errItem := b.restoreItem(item, false)
			if errItem != nil {
				return nil, errItem
			}
			input := gjson.GetBytes(out, "input").String()
			raw, _ = sjson.SetBytes(raw, "item_id", gjson.GetBytes(out, "id").String())
			raw, _ = sjson.DeleteBytes(raw, "arguments")
			raw, _ = sjson.SetBytes(raw, "type", "response.custom_tool_call_input.done")
			raw, _ = sjson.SetBytes(raw, "input", input)
			delta, _ := sjson.SetBytes(raw, "type", "response.custom_tool_call_input.delta")
			delta, _ = sjson.DeleteBytes(delta, "input")
			delta, _ = sjson.SetBytes(delta, "delta", input)
			call.done = true
			return b.number([][]byte{delta, raw}), nil
		}
	} else if strings.HasPrefix(kind, "response.function_call_arguments.") {
		return nil, fmt.Errorf("Muse arguments lack a declared call")
	}
	if response := gjson.GetBytes(raw, "response"); response.IsObject() {
		out, errResponse := b.Response([]byte(response.Raw))
		if errResponse != nil {
			return nil, errResponse
		}
		raw, err = sjson.SetRawBytes(raw, "response", out)
	}
	if err != nil {
		return nil, err
	}
	return b.number([][]byte{raw}), nil
}

func (b *MuseBridge) number(events [][]byte) [][]byte {
	for i, raw := range events {
		if gjson.GetBytes(raw, "sequence_number").Exists() {
			events[i], _ = sjson.SetBytes(raw, "sequence_number", b.sequence)
			b.sequence++
		}
	}
	return events
}
