package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const OfficialResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

type ResetCredit struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Type    string `json:"reset_type"`
	Granted any    `json:"granted_at"`
	Expires any    `json:"expires_at"`
}
type ResetCredits struct {
	Status    string        `json:"status"`
	Error     string        `json:"error,omitempty"`
	Available *int          `json:"available_count"`
	Credits   []ResetCredit `json:"credits"`
	Updated   *int64        `json:"updated_at,omitempty"`
}
type ResetSnapshot struct {
	OK       bool                    `json:"ok"`
	Accounts map[string]ResetCredits `json:"accounts"`
}

func resetText(value any, limit int) string {
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	if len(text) > limit || strings.IndexFunc(text, unicode.IsControl) >= 0 {
		return ""
	}
	return text
}
func resetDate(value any) any {
	if text, ok := value.(string); ok && len(text) <= 128 && strings.IndexFunc(text, unicode.IsControl) < 0 {
		return text
	}
	if v, ok := number(value); ok && v >= 0 && v <= 1e15 {
		return v
	}
	return nil
}
func ProjectResetCredits(raw []byte) (ResetCredits, error) {
	empty := ResetCredits{Status: "unavailable", Credits: []ResetCredit{}}
	if len(raw) == 0 || len(raw) > 262144 {
		return empty, errors.New("official reset response invalid")
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return empty, errors.New("official reset response invalid")
	}
	var source any
	for _, key := range []string{"credits", "rate_limit_reset_credits", "items", "data"} {
		if value, ok := doc[key]; ok {
			source = value
			break
		}
	}
	if nested, ok := source.(map[string]any); ok {
		source = nested["credits"]
		if source == nil {
			source = nested["items"]
		}
	}
	items, validRows := source.([]any)
	_, countPresent := doc["available_count"]
	if !validRows && (!countPresent || doc["available_count"] == nil) {
		return empty, errors.New("official reset response invalid")
	}
	result := ResetCredits{Status: "ok", Credits: []ResetCredit{}}
	for _, raw := range items[:min(len(items), 50)] {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, status := resetText(item["id"], 200), resetText(item["status"], 40)
		if id == "" || status == "" {
			continue
		}
		result.Credits = append(result.Credits, ResetCredit{ID: id, Status: status, Type: resetText(item["reset_type"], 80), Granted: resetDate(item["granted_at"]), Expires: resetDate(item["expires_at"])})
	}
	if value, exists := doc["available_count"]; exists && value != nil {
		if text, ok := value.(string); ok {
			parsed, err := strconv.ParseInt(text, 10, 64)
			if err == nil {
				result.Available = new(int(max(0, min(parsed, 10000))))
			}
		} else if v, ok := number(value); ok {
			result.Available = new(int(max(0, min(v, 10000))))
		}
	} else {
		count := 0
		for _, credit := range result.Credits {
			if strings.EqualFold(credit.Status, "available") {
				count++
			}
		}
		result.Available = new(count)
	}
	return result, nil
}

// FetchResetCredits is GET-only. No consumption/retry or OAuth refresh is hidden
// inside a quota-page read; the SDK supplies the exact account's current token.
func (f Fetcher) FetchResetCredits(ctx context.Context, credential Credential) (ResetCredits, error) {
	empty := ResetCredits{Status: "unavailable", Credits: []ResetCredit{}}
	if credential.Token == "" || len(credential.Token) > 65536 || credential.Workspace == "" || len(credential.Workspace) > 512 || strings.ContainsAny(credential.Token+credential.Workspace, "\r\n\x00") {
		return empty, errors.New("official credential unavailable")
	}
	factory := f.Transport
	if factory == nil {
		factory = defaultTransport
	}
	transport, err := factory(credential.Proxy)
	if err != nil {
		return empty, errors.New("official transport unavailable")
	}
	if idle, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer idle.CloseIdleConnections()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, OfficialResetCreditsURL, nil)
	if err != nil {
		return empty, errors.New("official request unavailable")
	}
	request.Header.Set("Authorization", "Bearer "+credential.Token)
	request.Header.Set("ChatGPT-Account-Id", credential.Workspace)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "codex_cli_rs/0.76.0")
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return empty, errors.New("official transport unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return empty, fmt.Errorf("official_http_%d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 262145))
	if err != nil {
		return empty, errors.New("official reset response unavailable")
	}
	return ProjectResetCredits(raw)
}

func CollectResetCredits(ctx context.Context, bindings []Binding, lookup Lookup, fetcher Fetcher, clock func() time.Time) ResetSnapshot {
	if clock == nil {
		clock = time.Now
	}
	rows := make([]ResetCredits, len(bindings))
	jobs := make(chan int)
	done := make(chan struct{}, 2)
	for worker := 0; worker < 2; worker++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for index := range jobs {
				row := ResetCredits{Status: "unavailable", Error: "auth_mapping_unavailable", Credits: []ResetCredit{}}
				credential, err := lookup(ctx, bindings[index])
				if err == nil {
					value, err := fetcher.FetchResetCredits(ctx, credential)
					if err != nil {
						row.Error = err.Error()
					} else {
						row = value
						row.Updated = new(clock().Unix())
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
	result := ResetSnapshot{OK: true, Accounts: map[string]ResetCredits{}}
	for index, row := range rows {
		result.Accounts[bindings[index].Prefix] = row
	}
	return result
}
