// Package accounts binds each public Codex prefix to exactly one persisted
// credential. Workspace IDs may be shared by different members of a team.
package accounts

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var prefixPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type Account struct {
	ID          string `json:"id"`
	Prefix      string `json:"prefix"`
	AuthFile    string `json:"auth_file"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	LegacySince *int64 `json:"legacy_since,omitempty"`
}

type Binding struct {
	Account
	AuthID   string
	disabled bool
	identity [32]byte
}

// Enabled reflects the registered file's disabled flag without exposing tokens.
func (b Binding) Enabled() bool { return !b.disabled }

// Registry is immutable after loading. Credentials stay in private auth files;
// only identity fingerprints and the established public account IDs are kept.
type Registry struct {
	byPrefix map[string]Binding
	byAuth   map[string]Binding
	models   map[string]bool
}

func Load(registryPath, authDir, modelsPath string) (*Registry, error) {
	raw, err := privatefile.Read(registryPath, 1024*1024, false)
	if err != nil {
		return nil, err
	}
	modelRaw, err := privatefile.Read(modelsPath, 1024*1024, false)
	if err != nil {
		return nil, err
	}
	return Parse(raw, authDir, modelRaw)
}

func Parse(raw []byte, authDir string, modelRaw []byte) (*Registry, error) {
	var doc struct {
		Accounts []Account `json:"accounts"`
	}
	if len(raw) > 1024*1024 || privatefile.Decode(raw, &doc) != nil || doc.Accounts == nil || len(doc.Accounts) > 128 {
		return nil, fmt.Errorf("invalid OAuth account registry")
	}
	info, err := os.Lstat(authDir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("OAuth auth directory is missing or is not a real directory")
	}
	models, err := parseModels(modelRaw)
	if err != nil {
		return nil, err
	}
	r := &Registry{byPrefix: map[string]Binding{}, byAuth: map[string]Binding{}, models: models}
	ids := map[string]bool{}
	identities := map[[32]byte]bool{}
	for _, account := range doc.Accounts {
		if strings.TrimSpace(account.ID) == "" || len(account.ID) > 256 || strings.ContainsAny(account.ID, "\r\n\x00") || !prefixPattern.MatchString(account.Prefix) || account.WorkspaceID == "" || account.LegacySince != nil && *account.LegacySince < 0 {
			return nil, fmt.Errorf("invalid OAuth account identity")
		}
		if account.AuthFile == "" || account.AuthFile != filepath.Base(account.AuthFile) || strings.ContainsAny(account.AuthFile, `/\`) || !strings.HasSuffix(strings.ToLower(account.AuthFile), ".json") {
			return nil, fmt.Errorf("OAuth credential reference must be a local JSON filename")
		}
		authID := account.AuthFile
		if runtime.GOOS == "windows" {
			authID = strings.ToLower(authID)
		}
		if ids[account.ID] {
			return nil, fmt.Errorf("duplicate OAuth account identity")
		}
		if _, exists := r.byPrefix[account.Prefix]; exists {
			return nil, fmt.Errorf("duplicate OAuth account prefix")
		}
		if _, exists := r.byAuth[authID]; exists {
			return nil, fmt.Errorf("one credential cannot belong to two OAuth accounts")
		}
		authRaw, err := privatefile.Read(filepath.Join(authDir, account.AuthFile), 4*1024*1024, true)
		if err != nil {
			return nil, err
		}
		var metadata map[string]any
		if privatefile.Decode(authRaw, &metadata) != nil {
			return nil, fmt.Errorf("invalid OAuth credential JSON")
		}
		identity, err := credentialIdentity(metadata)
		if err != nil || metadata["type"] != "codex" || metadata["prefix"] != account.Prefix || metadata["account_id"] != account.WorkspaceID {
			return nil, fmt.Errorf("OAuth credential does not match its registered prefix or workspace")
		}
		if identities[identity] {
			return nil, fmt.Errorf("the same OAuth member is registered more than once")
		}
		disabled, _ := metadata["disabled"].(bool)
		binding := Binding{Account: account, AuthID: authID, identity: identity, disabled: disabled}
		r.byPrefix[account.Prefix], r.byAuth[authID] = binding, binding
		ids[account.ID], identities[identity] = true, true
	}
	entries, err := os.ReadDir(authDir)
	if err != nil || len(entries) > 512 {
		return nil, fmt.Errorf("cannot validate bounded OAuth auth directory")
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		authID := entry.Name()
		if runtime.GOOS == "windows" {
			authID = strings.ToLower(authID)
		}
		if _, exists := r.byAuth[authID]; exists {
			continue
		}
		authRaw, err := privatefile.Read(filepath.Join(authDir, entry.Name()), 4*1024*1024, true)
		if err != nil {
			return nil, err
		}
		var metadata map[string]any
		if privatefile.Decode(authRaw, &metadata) != nil {
			return nil, fmt.Errorf("invalid unregistered auth file")
		}
		if metadata["type"] == "codex" && metadata["disabled"] != true {
			return nil, fmt.Errorf("an enabled Codex credential is missing from the account registry")
		}
	}
	if len(models) > 0 {
		if _, exists := r.byPrefix["codex"]; !exists {
			return nil, fmt.Errorf("bare Codex models require an explicit primary codex account")
		}
	}
	return r, nil
}

func credentialIdentity(metadata map[string]any) ([32]byte, error) {
	email, _ := metadata["email"].(string)
	workspace, _ := metadata["account_id"].(string)
	if strings.TrimSpace(email) == "" || workspace == "" {
		return [32]byte{}, fmt.Errorf("OAuth credential lacks a stable member identity")
	}
	subject := ""
	if token, _ := metadata["id_token"].(string); token != "" {
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			return [32]byte{}, fmt.Errorf("invalid OAuth identity token")
		}
		claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil || len(claimsRaw) > 64*1024 {
			return [32]byte{}, fmt.Errorf("invalid OAuth identity claims")
		}
		var claims struct {
			Sub   string `json:"sub"`
			Email string `json:"email"`
		}
		if privatefile.Decode(claimsRaw, &claims) != nil || claims.Sub == "" {
			return [32]byte{}, fmt.Errorf("OAuth identity claims lack a subject")
		}
		if claims.Email != "" && !strings.EqualFold(claims.Email, email) {
			return [32]byte{}, fmt.Errorf("OAuth credential member identity is inconsistent")
		}
		subject = claims.Sub
	}
	// This is an identity consistency check on already trusted local credentials,
	// not a claim that decoding a JWT verifies its cryptographic signature.
	encoded, _ := json.Marshal([]string{workspace, strings.ToLower(strings.TrimSpace(email)), subject})
	return sha256.Sum256(encoded), nil
}

func parseModels(raw []byte) (map[string]bool, error) {
	var list []json.RawMessage
	if len(raw) > 1024*1024 {
		return nil, fmt.Errorf("OAuth model list exceeds byte limit")
	}
	if privatefile.Decode(raw, &list) != nil {
		var doc struct {
			Models []json.RawMessage `json:"models"`
		}
		if privatefile.Decode(raw, &doc) != nil {
			return nil, fmt.Errorf("invalid exposed OAuth model list")
		}
		list = doc.Models
	}
	if list == nil || len(list) > 512 {
		return nil, fmt.Errorf("invalid exposed OAuth model list")
	}
	models := map[string]bool{}
	for _, item := range list {
		var name string
		if json.Unmarshal(item, &name) != nil {
			var object struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(item, &object) != nil {
				return nil, fmt.Errorf("invalid exposed OAuth model entry")
			}
			name = object.ID
		}
		name = strings.TrimPrefix(strings.TrimSpace(name), "codex/")
		if name == "" || len(name) > 256 || strings.ContainsAny(name, "/\r\n\x00()") {
			return nil, fmt.Errorf("invalid exposed OAuth model name")
		}
		models[name] = true
	}
	return models, nil
}

// Resolve returns an empty auth ID for unrelated subscription models. It never
// picks another OAuth account when the requested account is disabled/unavailable.
func (r *Registry) Resolve(model, path string) (string, string, error) {
	base := thinking.ParseSuffix(model).ModelName
	prefix, name, qualified := strings.Cut(base, "/")
	if !qualified {
		if !r.models[base] {
			return model, "", nil
		}
		prefix, name = "codex", base
	} else if _, known := r.byPrefix[prefix]; !known {
		return model, "", nil
	}
	if !strings.HasSuffix(path, "/responses") {
		return "", "", fmt.Errorf("OAuth model is not exposed for this endpoint")
	}
	if !r.models[name] {
		return "", "", fmt.Errorf("OAuth model is not in the exposed catalog")
	}
	binding := r.byPrefix[prefix]
	if binding.disabled {
		return "", "", fmt.Errorf("requested OAuth account is disabled")
	}
	if !qualified {
		model = prefix + "/" + model
	}
	return model, binding.AuthID, nil
}

func (r *Registry) ValidateAuth(auth *coreauth.Auth) error {
	if auth == nil {
		return fmt.Errorf("selected OAuth credential is missing")
	}
	binding, known := r.byAuth[auth.ID]
	if !known || auth.Provider != "codex" || auth.Prefix != binding.Prefix || auth.Disabled || auth.Status == coreauth.StatusDisabled || auth.Metadata["account_id"] != binding.WorkspaceID {
		return fmt.Errorf("selected OAuth credential does not match its registered account")
	}
	identity, err := credentialIdentity(auth.Metadata)
	if err != nil || identity != binding.identity {
		return fmt.Errorf("selected OAuth member identity changed without an account transaction")
	}
	return nil
}

func (r *Registry) Bindings() []Binding {
	items := make([]Binding, 0, len(r.byPrefix))
	for _, binding := range r.byPrefix {
		items = append(items, binding)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Prefix < items[j].Prefix })
	return items
}

func (r *Registry) PublicModelIDs() []string {
	items := []string{}
	for prefix := range r.byPrefix {
		for model := range r.models {
			if prefix == "codex" {
				items = append(items, model)
			} else {
				items = append(items, prefix+"/"+model)
			}
		}
	}
	sort.Strings(items)
	return items
}
