package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type recoveryKey struct{}

// WithRecovery enables a request-scoped policy, not a global translator fork.
// Upstream still owns namespace/custom-tool and Chat/Responses translation.
func WithRecovery(ctx context.Context) context.Context {
	return context.WithValue(ctx, recoveryKey{}, true)
}

type choiceState struct {
	mode     string
	buffer   strings.Builder
	chars    int
	finished bool
	overflow bool
}

type ChatBridge struct {
	declarations map[string]json.RawMessage
	allowed      map[string]bool
	states       map[int]*choiceState
	metadata     []byte
}

// NewChatBridge reads the FINAL translated request, after user payload rules.
// History and provider text cannot add declarations or relax tool_choice.
// A nil bridge leaves ordinary upstream executors byte-for-byte unchanged.
func NewChatBridge(ctx context.Context, translated []byte) (*ChatBridge, error) {
	if enabled, _ := ctx.Value(recoveryKey{}).(bool); !enabled {
		return nil, nil
	}
	bridge := &ChatBridge{declarations: map[string]json.RawMessage{}, allowed: map[string]bool{}, states: map[int]*choiceState{}}
	for _, tool := range gjson.GetBytes(translated, "tools").Array() {
		if tool.Get("type").String() != "function" {
			continue
		}
		name := tool.Get("function.name").String()
		schema := tool.Get("function.parameters")
		if name == "" || !schema.IsObject() {
			return nil, fmt.Errorf("OpenCode tool declaration is invalid")
		}
		if _, exists := bridge.declarations[name]; exists {
			return nil, fmt.Errorf("OpenCode tool declaration is ambiguous")
		}
		bridge.declarations[name] = json.RawMessage(schema.Raw)
		bridge.allowed[name] = true
	}
	choice := gjson.GetBytes(translated, "tool_choice")
	if choice.String() == "none" {
		bridge.allowed = map[string]bool{}
	} else if choice.IsObject() {
		name := choice.Get("function.name").String()
		bridge.allowed = map[string]bool{name: bridge.allowed[name]}
	}
	return bridge, nil
}

func (b *ChatBridge) recover(text, finish string) ([]ToolCall, bool) {
	if finish != "stop" && finish != "tool_calls" {
		return nil, false
	}
	calls, err := RecoverCalls(text, b.declarations, b.allowed)
	return calls, err == nil
}

// Response promotes only a complete, validated textual batch. Explicit native
// tool_calls take precedence; there is never a second batch from the text.
func (b *ChatBridge) Response(raw []byte) ([]byte, error) {
	if b == nil {
		return raw, nil
	}
	choices := gjson.GetBytes(raw, "choices").Array()
	if len(choices) > MaxCalls {
		return nil, fmt.Errorf("OpenCode choice count limit")
	}
	for index, choice := range choices {
		if len(choice.Get("message.tool_calls").Array()) > 0 {
			continue
		}
		text := choice.Get("message.content")
		if text.Type != gjson.String || strings.TrimSpace(text.String()) == "" || PrefixKind(text.String()) == "text" {
			continue
		}
		if PrefixKind(text.String()) == "pending" && !strings.Contains(text.String(), "DSML") {
			continue
		}
		base := fmt.Sprintf("choices.%d", index)
		calls, ok := b.recover(text.String(), choice.Get("finish_reason").String())
		var err error
		if ok {
			raw, err = sjson.SetBytes(raw, base+".message.tool_calls", calls)
			if err == nil {
				raw, err = sjson.SetBytes(raw, base+".message.content", nil)
			}
			if err == nil {
				raw, err = sjson.SetBytes(raw, base+".finish_reason", "tool_calls")
			}
		} else {
			raw, err = sjson.SetBytes(raw, base+".message.content", FormatError)
			if err == nil {
				raw, err = sjson.SetBytes(raw, base+".finish_reason", "stop")
			}
		}
		if err != nil {
			return nil, fmt.Errorf("cannot serialize OpenCode response")
		}
	}
	return raw, nil
}

// Event operates on one complete Chat SSE JSON event. DSML bytes stay buffered
// until the ENTIRE batch and terminal finish_reason have been validated.
func (b *ChatBridge) Event(raw []byte) ([]byte, error) {
	if b == nil {
		return raw, nil
	}
	choices := gjson.GetBytes(raw, "choices").Array()
	if len(choices) > MaxCalls {
		return nil, fmt.Errorf("OpenCode choice count limit")
	}
	// Keep only protocol identity, never output, reasoning or usage contents.
	metadata := map[string]any{}
	for _, key := range []string{"id", "object", "model", "created", "system_fingerprint"} {
		if value := gjson.GetBytes(raw, key); value.Exists() {
			metadata[key] = value.Value()
		}
	}
	if len(metadata) > 0 {
		b.metadata, _ = json.Marshal(metadata)
	}
	for pos, choice := range choices {
		index := int(choice.Get("index").Int())
		state := b.states[index]
		if state == nil {
			if len(b.states) >= MaxCalls {
				return nil, fmt.Errorf("OpenCode choice count limit")
			}
			state = &choiceState{mode: "pending"}
			b.states[index] = state
		}
		if state.finished {
			return nil, fmt.Errorf("OpenCode choice emitted after its terminal event")
		}
		base := fmt.Sprintf("choices.%d", pos)
		delta := choice.Get("delta")
		if len(delta.Get("tool_calls").Array()) > 0 {
			state.mode = "native"
			state.buffer.Reset()
		}
		finish := choice.Get("finish_reason").String()
		if state.mode != "text" && state.mode != "native" {
			text := delta.Get("content")
			var err error
			raw, err = sjson.DeleteBytes(raw, base+".delta.content")
			if err != nil {
				return nil, fmt.Errorf("cannot serialize OpenCode event")
			}
			if text.Exists() && text.Type != gjson.Null && text.Type != gjson.String {
				raw, err = sjson.SetRawBytes(raw, base+".delta.content", []byte(text.Raw))
				state.mode = "text"
				state.buffer.Reset()
			} else {
				if text.String() != "" && !state.overflow {
					state.chars += utf8.RuneCountInString(text.String())
					if state.chars > MaxChars {
						state.buffer.Reset()
						state.mode, state.overflow = "dsml", true
					} else {
						_, _ = state.buffer.WriteString(text.String())
						state.mode = PrefixKind(state.buffer.String())
					}
				}
				if state.mode == "text" {
					raw, err = sjson.SetBytes(raw, base+".delta.content", state.buffer.String())
					state.buffer.Reset()
				} else if finish != "" {
					source := state.buffer.String()
					if !state.overflow && state.mode == "pending" && !strings.Contains(source, "DSML") {
						if source != "" {
							raw, err = sjson.SetBytes(raw, base+".delta.content", source)
						}
					} else {
						calls, ok := b.recover(source, finish)
						if ok && !state.overflow {
							indexed := make([]map[string]any, len(calls))
							for i, call := range calls {
								indexed[i] = map[string]any{"index": i, "id": call.ID, "type": call.Type, "function": call.Function}
							}
							raw, err = sjson.SetBytes(raw, base+".delta.tool_calls", indexed)
							if err == nil {
								raw, err = sjson.SetBytes(raw, base+".finish_reason", "tool_calls")
							}
						} else {
							raw, err = sjson.SetBytes(raw, base+".delta.content", FormatError)
							if err == nil {
								raw, err = sjson.SetBytes(raw, base+".finish_reason", "stop")
							}
						}
					}
					state.buffer.Reset()
				}
			}
			if err != nil {
				return nil, fmt.Errorf("cannot serialize OpenCode event")
			}
		}
		if finish != "" {
			state.finished = true
		}
	}
	return raw, nil
}

// Finish does not authorize tools at [DONE]/EOF without a terminal choice.
// It emits a safe format error for held partial DSML rather than completing it.
func (b *ChatBridge) Finish() ([]byte, error) {
	if b == nil {
		return nil, nil
	}
	indices := []int{}
	for index, state := range b.states {
		if !state.finished && state.mode != "text" && state.mode != "native" {
			indices = append(indices, index)
		}
	}
	if len(indices) == 0 {
		return nil, nil
	}
	sort.Ints(indices)
	choices := make([]map[string]any, 0, len(indices))
	for _, index := range indices {
		choices = append(choices, map[string]any{"index": index, "delta": map[string]any{}, "finish_reason": "length"})
	}
	raw := b.metadata
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	raw, err := sjson.SetBytes(raw, "choices", choices)
	if err != nil {
		return nil, fmt.Errorf("cannot serialize OpenCode terminal event")
	}
	return b.Event(raw)
}
