package adminhelper

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
)

func (s *Service) claudeUsage(ctx context.Context) Result {
	// No caller-supplied origin, credential, headers or account selectors.
	raw, err := privatefile.Read(s.paths.Vault, 2*1024*1024, true)
	if err != nil {
		return failure("Claude provider credential unavailable")
	}
	doc, err := object(raw)
	var key string
	if err != nil || json.Unmarshal(doc["claude_native"], &key) != nil || key == "" {
		return failure("Claude provider credential unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8334/v1/claude/usage", nil)
	if err != nil {
		return failure("Claude quota unavailable")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	t := &http.Transport{Proxy: nil}
	defer t.CloseIdleConnections()
	client := &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := client.Do(request)
	if err != nil {
		return failure("Claude quota unavailable")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return failure("Claude quota unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16385))
	var value quota.ClaudeSnapshot
	if err != nil || len(body) > 16384 || json.Unmarshal(body, &value) != nil || value.Validate() != nil {
		return failure("Claude quota response invalid")
	}
	return Result{"ok": true, "account": value}
}
