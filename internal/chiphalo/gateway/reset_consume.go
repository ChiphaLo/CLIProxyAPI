package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type ResetCommand struct {
	Prefix    string `json:"prefix"`
	CreditID  string `json:"credit_id"`
	RequestID string `json:"redeem_request_id"`
	AccountID string `json:"expected_account_id"`
}

// ConsumeReset is private IPC only. The production host must explicitly enable
// its action; copied-auth shadow runtimes keep it disabled even for GUI peers.
// It is never executed by migration tests or automatic quota/model jobs.
func (l *LiveProfile) ConsumeReset(ctx context.Context, configPath string, manager *coreauth.Manager, fetcher quota.Fetcher, body []byte) (quota.Redemption, error) {
	var fields map[string]json.RawMessage
	if len(body) > 2048 || configbundle.DecodeDraft(body, &fields) != nil || len(fields) != 4 {
		return quota.Redemption{}, errors.New("invalid reset command")
	}
	for name := range fields {
		if name != "prefix" && name != "credit_id" && name != "redeem_request_id" && name != "expected_account_id" {
			return quota.Redemption{}, errors.New("unknown reset command field")
		}
	}
	var command ResetCommand
	if json.Unmarshal(body, &command) != nil {
		return quota.Redemption{}, errors.New("invalid reset command")
	}
	profile, err := l.Refresh()
	if err != nil || manager == nil {
		return quota.Redemption{}, errors.New("fixed reset member unavailable")
	}
	var authID, workspace string
	for _, b := range profile.Accounts.Bindings() {
		if b.Prefix == command.Prefix && b.ID == command.AccountID && b.Enabled() {
			authID, workspace = b.AuthID, b.WorkspaceID
		}
	}
	auth, found := manager.GetByID(authID)
	if !found || profile.Accounts.ValidateAuth(auth) != nil {
		return quota.Redemption{}, errors.New("fixed reset member unavailable")
	}
	raw, err := privatefile.Read(filepath.Join(l.paths.ConfigDir, "cliproxyapi.yaml"), 4*1024*1024, true)
	if err != nil {
		return quota.Redemption{}, errors.New("reset transport configuration unavailable")
	}
	if configPath != filepath.Join(l.paths.ConfigDir, "cliproxyapi.yaml") {
		return quota.Redemption{}, errors.New("reset config path is not the pinned publication")
	}
	cfg, err := config.ParseConfigBytes(raw)
	if err != nil {
		return quota.Redemption{}, errors.New("reset transport configuration unavailable")
	}
	token, _ := auth.Metadata["access_token"].(string)
	proxy := auth.ProxyURL
	if proxy == "" {
		proxy = cfg.ProxyURL
	}
	before := func() error {
		current, err := l.Refresh()
		if err != nil {
			return err
		}
		a, ok := manager.GetByID(authID)
		if !ok || current.Accounts.ValidateAuth(a) != nil {
			return errors.New("reset member revoked")
		}
		b, err := current.Accounts.BindingForAuth(authID)
		if err != nil || b.ID != command.AccountID || b.Prefix != command.Prefix {
			return errors.New("reset member changed")
		}
		return nil
	}
	return fetcher.RedeemResetCredit(ctx, quota.Credential{Token: token, Workspace: workspace, Proxy: proxy}, command.CreditID, command.RequestID, time.Now(), before), nil
}
