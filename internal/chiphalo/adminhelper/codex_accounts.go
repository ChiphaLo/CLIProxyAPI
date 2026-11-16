package adminhelper

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/catalogsync"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

type codexAccountDocument struct {
	Accounts []struct {
		ID     string `json:"id"`
		Prefix string `json:"prefix"`
		Label  string `json:"label"`
	} `json:"accounts"`
}
type codexExposureDocument struct {
	Models   []string                              `json:"models"`
	Accounts map[string][]string                   `json:"account_models"`
	Reviewed map[string][]string                   `json:"reviewed_account_models"`
	Settings map[string]catalogsync.AccountSetting `json:"account_settings"`
}
type codexDocuments struct {
	raw      map[string][]byte
	accounts codexAccountDocument
	exposed  codexExposureDocument
	metadata map[string]json.RawMessage
}

func (p *Applier) codexDocuments() (codexDocuments, error) {
	d := codexDocuments{raw: map[string][]byte{}}
	for _, name := range []string{configFiles[0], "oauth-accounts.json", catalogFiles[0], catalogFiles[1]} {
		raw, err := privatefile.Read(filepath.Join(p.options.ConfigDir, name), 4*1024*1024, name == configFiles[0])
		if err != nil {
			return d, errors.New("Codex inventory unavailable")
		}
		d.raw[name] = raw
	}
	var contextDoc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if configbundle.DecodeDraft(d.raw["oauth-accounts.json"], &d.accounts) != nil || configbundle.DecodeDraft(d.raw[catalogFiles[1]], &d.exposed) != nil || configbundle.DecodeDraft(d.raw[catalogFiles[0]], &contextDoc) != nil || d.exposed.Accounts == nil || contextDoc.Models == nil {
		return d, errors.New("account-local reviewed Codex inventory required")
	}
	if d.exposed.Reviewed == nil {
		d.exposed.Reviewed = d.exposed.Accounts
	}
	if d.exposed.Settings == nil {
		d.exposed.Settings = map[string]catalogsync.AccountSetting{}
	}
	d.metadata = contextDoc.Models
	if len(d.accounts.Accounts) == 0 || len(d.accounts.Accounts) > 128 || len(d.exposed.Reviewed) != len(d.accounts.Accounts) {
		return d, errors.New("Codex account inventory is incomplete")
	}
	for _, a := range d.accounts.Accounts {
		if names, ok := d.exposed.Reviewed[a.ID]; !ok || names == nil || len(names) > 512 {
			return d, errors.New("Codex account inventory is incomplete")
		}
	}
	return d, nil
}
func codexPublicModel(prefix, name string) string {
	if prefix == "codex" {
		return name
	}
	return prefix + "/" + name
}

func (s *Service) codexAccounts(ctx context.Context) Result {
	if s.apply == nil {
		return failure("Codex account settings unavailable")
	}
	d, err := s.apply.codexDocuments()
	if err != nil {
		return failure(err.Error())
	}
	rows := []any{}
	for _, a := range d.accounts.Accounts {
		setting, custom := d.exposed.Settings[a.ID]
		if !custom {
			setting = catalogsync.AccountSetting{Name: a.Label, Enabled: true, DisabledModels: []string{}}
			if setting.Name == "" {
				setting.Name = a.Prefix
			}
		}
		models := []any{}
		for _, name := range d.exposed.Reviewed[a.ID] {
			id := codexPublicModel(a.Prefix, name)
			raw := d.metadata[id]
			if raw == nil && a.Prefix == "codex" {
				raw = d.metadata["codex/"+name]
			}
			// Project only public capability fields, never raw context/credential data.
			meta := ojson.ParseBytes(raw)
			levels := []string{}
			meta.Get("supported_reasoning_levels").ForEach(func(_, v ojson.Result) bool {
				if v.Type == ojson.String {
					levels = append(levels, v.String())
				} else if v.Get("effort").String() != "" {
					levels = append(levels, v.Get("effort").String())
				}
				return true
			})
			models = append(models, map[string]any{"name": name, "id": id, "enabled": !slices.Contains(setting.DisabledModels, name), "contextWindow": meta.Get("context_window").Int(), "thinking": levels})
		}
		rows = append(rows, map[string]any{"id": a.ID, "prefix": a.Prefix, "name": setting.Name, "enabled": setting.Enabled, "models": models})
	}
	return Result{"ok": true, "accounts": rows, "revision": hash(d.raw[catalogFiles[1]])}
}

func (s *Service) updateCodexAccount(ctx context.Context, payload map[string]json.RawMessage) Result {
	if s.apply == nil || s.inventory == nil {
		return failure("Codex management unavailable")
	}
	if allowed(payload, "account_id", "revision", "name", "enabled", "disabled_models") != nil {
		return failure("unknown Codex settings fields")
	}
	var id, revision string
	var setting catalogsync.AccountSetting
	if json.Unmarshal(payload["account_id"], &id) != nil || json.Unmarshal(payload["revision"], &revision) != nil || !validDigest(revision) || json.Unmarshal(payload["name"], &setting.Name) != nil || json.Unmarshal(payload["enabled"], &setting.Enabled) != nil || string(payload["enabled"]) == "null" || json.Unmarshal(payload["disabled_models"], &setting.DisabledModels) != nil {
		return failure("invalid Codex settings")
	}
	setting.Name = strings.TrimSpace(setting.Name)
	d, err := s.apply.codexDocuments()
	if err != nil {
		return failure(err.Error())
	}
	names, known := d.exposed.Reviewed[id]
	if !known || revision != hash(d.raw[catalogFiles[1]]) {
		return failure("Codex settings changed; reload before saving")
	}
	if err = catalogsync.ValidateAccountSetting(setting, names); err != nil {
		return failure(err.Error())
	}
	live, err := s.inventory.Read(ctx, "/accounts")
	if err != nil || live["ok"] != true {
		return failure("registered Codex accounts unavailable")
	}
	physical := map[string]bool{}
	prefixes := map[string]string{}
	items, _ := live["accounts"].([]any)
	for _, v := range items {
		a, _ := v.(map[string]any)
		aid, _ := a["id"].(string)
		prefix, _ := a["prefix"].(string)
		physical[aid] = a["enabled"] == true
		prefixes[aid] = prefix
	}
	for _, a := range d.accounts.Accounts {
		if prefixes[a.ID] != a.Prefix {
			return failure("Codex source binding changed")
		}
	}
	d.exposed.Settings[id] = setting
	selected := catalogsync.SelectedAccountModels(d.exposed.Reviewed, d.exposed.Settings)
	doc := ojson.NewDoc(d.raw[catalogFiles[1]])
	if doc.Set("reviewed_account_models", d.exposed.Reviewed) != nil || doc.Set("account_settings", d.exposed.Settings) != nil || doc.Set("account_models", selected) != nil {
		return failure("cannot encode Codex settings")
	}
	actual, err := s.apply.options.Backend.Catalog(ctx)
	if err != nil {
		return failure("current model catalog unavailable")
	}
	oauthIDs := map[string]bool{}
	set := map[string]bool{}
	for _, a := range d.accounts.Accounts {
		for _, name := range d.exposed.Models {
			oauthIDs[codexPublicModel(a.Prefix, name)] = true
			oauthIDs[a.Prefix+"/"+name] = true
		}
		if physical[a.ID] {
			for _, name := range selected[a.ID] {
				set[codexPublicModel(a.Prefix, name)] = true
			}
		}
	}
	for _, model := range actual {
		if !oauthIDs[model] {
			set[model] = true
		}
	}
	next := []string{}
	for model := range set {
		next = append(next, model)
	}
	slices.Sort(next)
	p := catalogsync.Publication{Config: hash(d.raw[configFiles[0]]), Accounts: hash(d.raw["oauth-accounts.json"]), OldContext: hash(d.raw[catalogFiles[0]]), OldExposed: revision, Context: d.raw[catalogFiles[0]], Exposed: doc.Bytes(), OldModels: actual, NewModels: next, Changed: true}
	result := s.apply.PublishCatalog(ctx, p)
	if result["ok"] == true {
		result["output"] = "Codex account settings saved and applied"
	}
	return result
}

func (s *Service) codexAccountModels(ctx context.Context, payload map[string]json.RawMessage) Result {
	if s.apply == nil || s.inventory == nil || allowed(payload, "account_id") != nil {
		return failure("Codex model discovery unavailable")
	}
	var id string
	if json.Unmarshal(payload["account_id"], &id) != nil {
		return failure("registered Codex account required")
	}
	d, err := s.apply.codexDocuments()
	if err != nil {
		return failure(err.Error())
	}
	prefix := ""
	for _, a := range d.accounts.Accounts {
		if a.ID == id {
			prefix = a.Prefix
		}
	}
	if prefix == "" {
		return failure("registered Codex account required")
	}
	var snapshot catalogsync.Snapshot
	if s.inventory.ReadCodexModels(ctx, id, &snapshot) != nil || !snapshot.OK || len(snapshot.Accounts) != 1 {
		return failure("official Codex model list unavailable; configuration was not changed")
	}
	a := snapshot.Accounts[0]
	if a.ID != id || a.Prefix != prefix || a.Status != "ok" || a.Models == nil || len(a.Models) > 512 {
		return failure("official Codex model list unavailable; configuration was not changed")
	}
	return Result{"ok": true, "account_id": id, "prefix": prefix, "models": a.Models, "count": len(a.Models), "source": catalogsync.OfficialModelsURL, "published": false}
}
