// Package catalogsync discovers and reviews account-local OAuth models without
// rotating credentials or making public model-list reads a configuration writer.
package catalogsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

const OfficialModelsURL = "https://chatgpt.com/backend-api/codex/models"
const DefaultClientVersion = "0.159.0"

var versionPattern = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$`)
var namePattern = regexp.MustCompile(`^gpt-[A-Za-z0-9._-]{1,120}$`)
var userAgentVersionPattern = regexp.MustCompile(`(?:^|\s)(?:codex_cli_rs|codex-desktop)/([0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4})`)

// ClientVersion uses the configured trusted Codex header when present, otherwise
// the captured upstream fetch utility's version. It never accepts request input.
func ClientVersion(userAgent string) string {
	match := userAgentVersionPattern.FindStringSubmatch(userAgent)
	if len(match) == 2 {
		return match[1]
	}
	return DefaultClientVersion
}

type Model struct {
	Name            string   `json:"name"`
	Display         string   `json:"display_name"`
	Window          int64    `json:"context_window"`
	MaxWindow       int64    `json:"max_context_window"`
	DefaultThinking string   `json:"default_reasoning_level"`
	Thinking        []string `json:"supported_reasoning_levels"`
	Inputs          []string `json:"input_modalities"`
	Visibility      string   `json:"visibility"`
	MultiAgent      string   `json:"multi_agent_version,omitempty"`
}
type Account struct {
	ID     string  `json:"id"`
	Prefix string  `json:"prefix"`
	Status string  `json:"status"`
	Error  string  `json:"error,omitempty"`
	Models []Model `json:"models"`
}
type Snapshot struct {
	OK       bool      `json:"ok"`
	Accounts []Account `json:"accounts"`
	At       int64     `json:"generated_at"`
}
type Fetcher struct {
	Transport     quota.TransportFactory
	ClientVersion string
}

func safeText(value any, limit int) string {
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	if len(text) > limit || strings.IndexFunc(text, unicode.IsControl) >= 0 {
		return ""
	}
	return text
}
func window(value any) int64 {
	if n, ok := value.(float64); ok && n >= 1024 && n <= 100000000 && n == float64(int64(n)) {
		return int64(n)
	}
	return 0
}
func Eligible(name string) bool {
	return namePattern.MatchString(name) && !strings.HasPrefix(name, "gpt-image") && !Retired(name)
}
func Retired(name string) bool {
	return name == "gpt-5.5" || strings.HasPrefix(name, "gpt-5.5-") || name == "gpt-5.6" || strings.HasPrefix(name, "gpt-5.6-") || name == "gpt-6-sol"
}
func thinkingLevel(name string) bool {
	for _, known := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		if name == known {
			return true
		}
	}
	return false
}
func Project(raw []byte) ([]Model, error) {
	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if len(raw) > 2*1024*1024 || configbundle.DecodeDraft(raw, &payload) != nil || payload.Models == nil || len(payload.Models) > 512 {
		return nil, errors.New("official catalog response invalid")
	}
	models, seen := []Model{}, map[string]bool{}
	for _, value := range payload.Models {
		name := safeText(value["slug"], 128)
		if name == "" {
			name = safeText(value["id"], 128)
		}
		if !Eligible(name) {
			continue
		}
		if seen[name] {
			return nil, errors.New("official catalog has duplicate model identity")
		}
		seen[name] = true
		model := Model{Name: name, Display: safeText(value["display_name"], 256), Window: window(value["context_window"]), MaxWindow: window(value["max_context_window"]), Visibility: safeText(value["visibility"], 32), DefaultThinking: safeText(value["default_reasoning_level"], 16), Thinking: []string{}, Inputs: []string{}}
		if model.MaxWindow == 0 {
			model.MaxWindow = model.Window
		}
		if model.Window > model.MaxWindow {
			return nil, errors.New("official catalog context bounds invalid")
		}
		levels, _ := value["supported_reasoning_levels"].([]any)
		seenLevels := map[string]bool{}
		for _, item := range levels {
			if object, ok := item.(map[string]any); ok {
				item = object["effort"]
			}
			level := safeText(item, 16)
			if thinkingLevel(level) && !seenLevels[level] {
				seenLevels[level] = true
				model.Thinking = append(model.Thinking, level)
			}
		}
		if model.DefaultThinking != "" && !seenLevels[model.DefaultThinking] {
			model.DefaultThinking = ""
		}
		inputs, _ := value["input_modalities"].([]any)
		seenInputs := map[string]bool{}
		for _, item := range inputs {
			input := safeText(item, 16)
			if (input == "text" || input == "image") && !seenInputs[input] {
				seenInputs[input] = true
				model.Inputs = append(model.Inputs, input)
			}
		}
		if len(model.Inputs) == 0 {
			model.Inputs = []string{"text"}
		}
		if version := safeText(value["multi_agent_version"], 8); version == "v1" || version == "v2" {
			model.MultiAgent = version
		}
		models = append(models, model)
	}
	return models, nil
}

func (f Fetcher) Fetch(ctx context.Context, credential quota.Credential) ([]Model, error) {
	if credential.Token == "" || len(credential.Token) > 65536 || credential.Workspace == "" || len(credential.Workspace) > 512 || strings.ContainsAny(credential.Token+credential.Workspace, "\r\n\x00") {
		return nil, errors.New("registered official credential unavailable")
	}
	version := f.ClientVersion
	if version == "" {
		version = DefaultClientVersion
	}
	if !versionPattern.MatchString(version) {
		return nil, errors.New("catalog client version invalid")
	}
	factory := f.Transport
	if factory == nil {
		factory = quota.OfficialTransport
	}
	transport, err := factory(credential.Proxy)
	if err != nil {
		return nil, errors.New("official catalog transport unavailable")
	}
	if idle, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer idle.CloseIdleConnections()
	}
	u, _ := url.Parse(OfficialModelsURL)
	u.RawQuery = url.Values{"client_version": []string{version}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("official catalog request unavailable")
	}
	request.Header.Set("Authorization", "Bearer "+credential.Token)
	request.Header.Set("ChatGPT-Account-Id", credential.Workspace)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Originator", "codex_cli_rs")
	request.Header.Set("User-Agent", "codex_cli_rs/"+version)
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("official catalog transport unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("official_catalog_http_%d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil {
		return nil, errors.New("official catalog response unavailable")
	}
	return Project(raw)
}

// Discover is read-only and uses each exact SDK-bound member, never an auth-file
// scan, credential refresh or first-available account fallback.
func Discover(ctx context.Context, bindings []quota.Binding, lookup quota.Lookup, fetcher Fetcher, clock func() time.Time) Snapshot {
	if clock == nil {
		clock = time.Now
	}
	rows := make([]Account, len(bindings))
	jobs, done := make(chan int), make(chan struct{}, 2)
	for worker := 0; worker < 2; worker++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for index := range jobs {
				binding := bindings[index]
				row := Account{ID: binding.ID, Prefix: binding.Prefix, Status: "unavailable", Error: "registered credential unavailable", Models: []Model{}}
				credential, err := lookup(ctx, binding)
				if err == nil {
					if models, err := fetcher.Fetch(ctx, credential); err == nil {
						row.Models, row.Status, row.Error = models, "ok", ""
					} else {
						row.Error = err.Error()
					}
				}
				rows[index] = row
			}
		}()
	}
	for index := range bindings {
		jobs <- index
	}
	close(jobs)
	<-done
	<-done
	return Snapshot{OK: true, Accounts: rows, At: clock().Unix()}
}
