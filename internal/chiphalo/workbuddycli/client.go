package workbuddycli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

type Config struct {
	Node           string
	Entry          string
	ProductFile    string
	CredentialFile string
	Home           string
	ExpectedUID    string
}

type Result struct {
	Text          string
	Calls         []ToolCall
	Usage         json.RawMessage
	Model         string
	FinishReason  string
	DeniedActions int
}

type pendingCall struct {
	title string
	input json.RawMessage
	ready bool
}

// Generate runs exactly one ephemeral official-client session. Tool proposals
// are handed back to the API caller; no CLI tool request is ever approved.
func (cfg Config) Generate(ctx context.Context, r Request, delta func(string) error) (*Result, error) {
	p, err := prepare(r)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "linux" {
		return nil, errors.New("official-client worker must run in its isolated Linux service")
	}
	for _, path := range []string{cfg.Node, cfg.Entry, cfg.ProductFile, cfg.CredentialFile, cfg.Home} {
		if !filepath.IsAbs(path) {
			return nil, errors.New("official-client worker paths must be absolute")
		}
	}
	raw, err := privatefile.Read(cfg.CredentialFile, 128<<10, true)
	if err != nil {
		return nil, err
	}
	var credential struct {
		AccessToken string `json:"accessToken"`
		TestToken   string `json:"access_token"`
		UID         string `json:"uid"`
		ExpiresAt   int64  `json:"expiresAt"`
	}
	if privatefile.Decode(raw, &credential) != nil {
		return nil, errors.New("cannot read official-client credential")
	}
	token := credential.AccessToken
	if token == "" {
		token = credential.TestToken
	}
	if token == "" || strings.ContainsAny(token, "\x00\r\n") {
		return nil, errors.New("official-client credential has no valid access token")
	}
	if cfg.ExpectedUID != "" && credential.UID != cfg.ExpectedUID {
		return nil, errors.New("official-client account identity does not match its binding")
	}
	if cfg.ExpectedUID != "" && credential.ExpiresAt <= time.Now().Unix() {
		return nil, errors.New("official-client access projection is expired or lacks expiry")
	}
	work, err := os.MkdirTemp(cfg.Home, "request-")
	if err != nil {
		return nil, errors.New("cannot create private official-client workspace")
	}
	defer func() { _ = os.RemoveAll(work) }()
	promptFile := filepath.Join(work, "system.txt")
	if err = os.WriteFile(promptFile, []byte(p.system), 0600); err != nil {
		return nil, errors.New("cannot write private official-client instructions")
	}
	// Only the documented OAuth environment and the official product profile
	// are supplied. Never inherit caller credentials, API URLs, proxies, hooks,
	// MCP definitions, or system/user settings into this inference process.
	env := []string{
		"PATH=/usr/bin:/bin", "HOME=" + work, "TZ=Asia/Singapore",
		"CODEBUDDY_AUTH_TOKEN=" + token,
		"ACC_PRODUCT_CONFIG_PATH=" + cfg.ProductFile,
		"CODEBUDDY_CONFIG_DIR=" + filepath.Join(work, "config"),
		"CODEBUDDY_DISABLE_PLUGIN_INSTALLS=1",
		"CODEBUDDY_SKIP_BUILTIN_MARKETPLACE=1",
		"CODEBUDDY_DISABLE_EXTENDED_PLUGIN_HOOKS=1",
		"CODEBUDDY_AUTO_UPDATE_THIRD_PARTY_MARKETPLACES=0",
		"CODEBUDDY_CODE_DISABLE_TERMINAL_TITLE=1",
		"CODEBUDDY_PRE_MESSAGE_COMPACT=0",
	}
	env = append(env, outputLimitEnvironment(r)...)
	args := []string{
		"--acp", "--acp-transport", "stdio", "--model", r.Model,
		"--tools", "DelegateTool", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--setting-sources", "", "--no-session-persistence", "--max-turns", "1",
		"--autocompact", "1m",
		"--system-prompt-file", promptFile,
	}
	if r.ReasoningEffort != "" {
		args = append(args, "--effort", r.ReasoningEffort)
	}
	c, err := start(ctx, cfg.Node, cfg.Entry, work, args, env)
	if err != nil {
		return nil, err
	}
	defer c.close()
	result := &Result{Model: r.Model, FinishReason: "stop"}
	calls, order := map[string]*pendingCall{}, []string{}
	collect := false
	c.observe = func(method string, params json.RawMessage) error {
		if method != "session/update" || !collect {
			return nil
		}
		var notification struct {
			Update struct {
				Kind    string                     `json:"sessionUpdate"`
				Title   string                     `json:"title"`
				CallID  string                     `json:"toolCallId"`
				Status  string                     `json:"status"`
				Input   json.RawMessage            `json:"rawInput"`
				Content json.RawMessage            `json:"content"`
				Meta    map[string]json.RawMessage `json:"_meta"`
			} `json:"update"`
		}
		if json.Unmarshal(params, &notification) != nil {
			return errors.New("malformed official-client update")
		}
		u := notification.Update
		switch u.Kind {
		case "agent_message_chunk":
			var content struct{ Type, Text string }
			if json.Unmarshal(u.Content, &content) != nil || content.Type != "text" {
				return errors.New("unsupported official-client output content")
			}
			result.Text += content.Text
			if len(result.Text) > 16<<20 {
				return errors.New("official-client output exceeds resource bound")
			}
			if delta != nil && content.Text != "" {
				return delta(content.Text)
			}
		case "tool_call", "tool_call_update":
			if u.CallID == "" {
				return errors.New("official-client tool proposal lacks identity")
			}
			v := calls[u.CallID]
			if v == nil {
				if len(calls) >= 128 {
					return errors.New("official-client tool batch exceeds resource bound")
				}
				v = &pendingCall{}
				calls[u.CallID] = v
				order = append(order, u.CallID)
			}
			if u.Title != "" {
				v.title = u.Title
			}
			if len(u.Input) != 0 && string(u.Input) != "null" {
				v.input = append(json.RawMessage(nil), u.Input...)
			}
			if u.Status == "pending" {
				v.ready = true
			}
		case "usage_update":
			if usage := u.Meta["usage"]; len(usage) != 0 && string(usage) != "null" {
				result.Usage = append(json.RawMessage(nil), usage...)
			}
		}
		return nil
	}
	initial, err := c.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]any{"name": "CPA official-client adapter", "version": "0.1"}})
	if err != nil {
		return nil, err
	}
	var capabilities struct {
		Agent struct {
			Delegate bool `json:"delegateToolsSupport"`
		} `json:"agentCapabilities"`
	}
	if json.Unmarshal(initial, &capabilities) != nil || !capabilities.Agent.Delegate {
		return nil, errors.New("official client lacks required delegation capability")
	}
	if len(p.tools) != 0 {
		if _, err = c.call(ctx, "_codebuddy.ai/delegateToolsChanged", map[string]any{"changeType": "added", "providerId": "cpa-downstream", "tools": p.tools}); err != nil {
			return nil, err
		}
	}
	created, err := c.call(ctx, "session/new", map[string]any{"cwd": work, "mcpServers": []any{}})
	if err != nil {
		return nil, err
	}
	var session struct {
		ID string `json:"sessionId"`
	}
	if json.Unmarshal(created, &session) != nil || session.ID == "" {
		return nil, errors.New("official client did not create a session")
	}
	c.sessionID = session.ID
	if len(p.history) != 0 {
		if _, err = c.call(ctx, "session/inject_history", map[string]any{"sessionId": session.ID, "messages": p.history}); err != nil {
			return nil, err
		}
	}
	collect = true
	finished, err := c.call(ctx, "session/prompt", map[string]any{"sessionId": session.ID, "prompt": []any{map[string]any{"type": "text", "text": p.prompt}}})
	if err != nil {
		return nil, err
	}
	var stop struct {
		Reason string `json:"stopReason"`
		Meta   struct {
			Outcome string `json:"codebuddy.ai/outcome"`
		} `json:"_meta"`
	}
	if json.Unmarshal(finished, &stop) != nil {
		return nil, errors.New("invalid official-client completion")
	}
	for _, id := range order {
		call := calls[id]
		if !call.ready || call.title != "DelegateTool" {
			return nil, errors.New("official client proposed an incomplete or forbidden tool")
		}
		var proposal struct {
			Name  string         `json:"toolId"`
			Input map[string]any `json:"input"`
		}
		if json.Unmarshal(call.input, &proposal) != nil || proposal.Input == nil || p.schemas[proposal.Name] == nil {
			return nil, errors.New("official client proposed an undeclared or malformed tool")
		}
		if p.schemas[proposal.Name].Validate(proposal.Input) != nil {
			return nil, errors.New("official-client tool arguments violate the downstream schema")
		}
		arguments, _ := json.Marshal(proposal.Input)
		result.Calls = append(result.Calls, ToolCall{ID: id, Type: "function", Function: Function{Name: proposal.Name, Arguments: string(arguments)}})
	}
	if len(result.Calls) != 0 {
		if c.denials == 0 || stop.Reason != "cancelled" || stop.Meta.Outcome != "PERMISSION_DENIED" && !(c.handoffCancelled && stop.Meta.Outcome == "CANCELLED") {
			return nil, errors.New("official-client tool handoff was not safely intercepted")
		}
		result.FinishReason = "tool_calls"
	} else {
		result.FinishReason, err = textFinishReason(stop.Reason, p.required)
		if err != nil {
			return nil, err
		}
	}
	if len(result.Usage) == 0 {
		return nil, errors.New("official client did not report final token usage")
	}
	var counts struct {
		Prompt     *int64 `json:"prompt_tokens"`
		Completion *int64 `json:"completion_tokens"`
		Total      *int64 `json:"total_tokens"`
	}
	if json.Unmarshal(result.Usage, &counts) != nil || counts.Prompt == nil || counts.Completion == nil || counts.Total == nil || *counts.Prompt < 0 || *counts.Completion < 0 || *counts.Total < 0 {
		return nil, errors.New("official client reported invalid final token usage")
	}
	result.DeniedActions = c.denials
	return result, nil
}

// The official CLI applies this per-process bound to its model maxTokens.
// Never truncate generated text locally: that would corrupt tool proposals and usage.
func outputLimitEnvironment(r Request) []string {
	if r.MaxTokens == 0 {
		return nil
	}
	return []string{"CODEBUDDY_CODE_MAX_OUTPUT_TOKENS=" + strconv.Itoa(r.MaxTokens)}
}

func textFinishReason(reason string, required bool) (string, error) {
	if !required {
		switch reason {
		case "end_turn":
			return "stop", nil
		case "max_tokens":
			return "length", nil
		}
	}
	return "", errors.New("official client did not complete the requested turn")
}
