package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func collectAntigravityQuota(ctx context.Context, manager *coreauth.Manager, proxy string, fetcher quota.Fetcher) []quota.AntigravityAccount {
	rows := []quota.AntigravityAccount{}
	for _, auth := range manager.List() {
		if auth.Provider != "antigravity" || auth.Disabled || len(rows) >= 16 {
			continue
		}
		project, _ := auth.Metadata["project_id"].(string)
		email, _ := auth.Metadata["email"].(string)
		token, _ := auth.Metadata["access_token"].(string)
		identity := sha256.Sum256([]byte(auth.ID + "\x00" + project + "\x00" + email))
		prefix := auth.Prefix
		if prefix == "" {
			prefix = "antigravity"
		}
		row := quota.AntigravityAccount{ID: hex.EncodeToString(identity[:]), Prefix: prefix, Checked: time.Now().Unix(), Status: "unavailable", Models: []quota.AntigravityModel{}}
		allowed := map[string]bool{}
		for _, info := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
			if info != nil {
				name := strings.TrimPrefix(info.ID, auth.Prefix+"/")
				if strings.HasPrefix(name, "gemini-") {
					allowed[name] = true
				}
			}
		}
		route := auth.ProxyURL
		if route == "" {
			route = proxy
		}
		credential := quota.Credential{Token: token, Workspace: project, Proxy: route}
		summary, summaryErr := fetcher.FetchAntigravitySummary(ctx, credential)
		if summaryErr == nil {
			row.Status = "ok"
			row.Gemini = summary
			row.Updated = summary.Updated
		} else {
			row.Gemini = &quota.AntigravityGeminiQuota{Status: "unavailable", Checked: time.Now().Unix()}
			models, err := fetcher.FetchAntigravity(ctx, credential, allowed)
			if err == nil {
				row.Status = "ok"
				row.Models = models
				row.Updated = new(time.Now().Unix())
			}
		}
		rows = append(rows, row)
	}
	return rows
}
