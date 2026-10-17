// Package clientkeys preserves CPA's existing bearer admin key and SHA-256
// issued-key database. Plaintext issued keys are never needed or persisted.
package clientkeys

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/nativegemini"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
)

const ProviderID = "chiphalo-legacy-keys"

// ValidateDatabase checks issued-key policy syntax without an admin credential.
// The running gateway additionally checks collisions with its actual admin key.
func ValidateDatabase(raw []byte) error {
	p := &Provider{}
	return p.Reload(raw)
}

var identityPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type Entry struct {
	ID               string   `json:"id"`
	Hash             string   `json:"hash"`
	Enabled          *bool    `json:"enabled"`
	ExpiresAt        *int64   `json:"expiresAt"`
	ModelPatterns    []string `json:"modelPatterns"`
	ModelPatternsOld []string `json:"model_patterns"`
	Prefixes         []string `json:"providerPrefixes"`
	PrefixesOld      []string `json:"provider_prefixes"`
	RPM              int      `json:"rpm"`
	MaxConcurrent    int      `json:"maxConcurrent"`
}

type policy struct {
	entry    Entry
	patterns []*regexp.Regexp
}

type snapshot struct {
	byHash map[[32]byte]*policy
	byID   map[string]*policy
}

type Provider struct {
	admin [32]byte
	state atomic.Pointer[snapshot]
	now   func() time.Time
}

// New rejects ambiguous or malformed policies rather than silently granting
// unrestricted access. Extra legacy display fields are intentionally ignored.
func New(admin string, raw []byte) (*Provider, error) {
	if strings.TrimSpace(admin) == "" || len(admin) > 4096 {
		return nil, fmt.Errorf("a nonempty existing CPA admin key is required")
	}
	p := &Provider{admin: sha256.Sum256([]byte(admin)), now: time.Now}
	if err := p.Reload(raw); err != nil {
		return nil, err
	}
	return p, nil
}

func Load(admin, path string) (*Provider, error) {
	raw, err := privatefile.Read(path, 2*1024*1024, true)
	if err != nil {
		return nil, err
	}
	return New(admin, raw)
}

// Reload validates a complete candidate before publishing one immutable
// snapshot. Invalid candidates never partially mutate the active key policy.
func (p *Provider) Reload(raw []byte) error {
	var doc struct {
		Keys []Entry `json:"keys"`
	}
	if len(raw) > 2*1024*1024 || privatefile.Decode(raw, &doc) != nil || len(doc.Keys) > 10000 || doc.Keys == nil {
		return fmt.Errorf("invalid issued-key database")
	}
	next := &snapshot{byHash: map[[32]byte]*policy{}, byID: map[string]*policy{}}
	for _, entry := range doc.Keys {
		if !identityPattern.MatchString(entry.ID) || entry.ID == "admin" || len(entry.Hash) != 64 || entry.RPM < 0 || entry.RPM > 10000 || entry.MaxConcurrent < 0 || entry.MaxConcurrent > 64 || entry.ExpiresAt != nil && *entry.ExpiresAt < 0 {
			return fmt.Errorf("invalid issued-key identity or policy")
		}
		digestBytes, err := hex.DecodeString(entry.Hash)
		if err != nil || len(digestBytes) != 32 {
			return fmt.Errorf("invalid issued-key digest")
		}
		var digest [32]byte
		copy(digest[:], digestBytes)
		if _, exists := next.byID[entry.ID]; exists {
			return fmt.Errorf("duplicate issued-key identity")
		}
		if _, exists := next.byHash[digest]; exists || subtle.ConstantTimeCompare(digest[:], p.admin[:]) == 1 {
			return fmt.Errorf("ambiguous issued-key digest")
		}
		if len(entry.ModelPatterns) == 0 {
			entry.ModelPatterns = entry.ModelPatternsOld
		}
		if len(entry.Prefixes) == 0 {
			entry.Prefixes = entry.PrefixesOld
		}
		if len(entry.ModelPatterns) > 128 || len(entry.Prefixes) > 128 {
			return fmt.Errorf("issued-key policy exceeds resource limits")
		}
		if entry.RPM == 0 {
			entry.RPM = 120
		}
		if entry.MaxConcurrent == 0 {
			entry.MaxConcurrent = 8
		}
		candidate := &policy{entry: entry}
		for _, prefix := range entry.Prefixes {
			if !identityPattern.MatchString(prefix) {
				return fmt.Errorf("invalid issued-key provider prefix")
			}
		}
		for _, pattern := range entry.ModelPatterns {
			compiled, err := compileGlob(pattern)
			if err != nil {
				return err
			}
			candidate.patterns = append(candidate.patterns, compiled)
		}
		next.byID[entry.ID], next.byHash[digest] = candidate, candidate
	}
	p.state.Store(next)
	return nil
}

func (p *Provider) Identifier() string { return ProviderID }

func (p *Provider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil || p.state.Load() == nil {
		return nil, sdkaccess.NewInternalAuthError("CPA key policy unavailable", nil)
	}
	values, google := r.Header.Values("Authorization"), r.Header.Values("X-Goog-Api-Key")
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || query.Has("key") {
		return nil, sdkaccess.NewInvalidCredentialError()
	}
	if len(values) == 0 && len(google) == 0 {
		return nil, sdkaccess.NewNoCredentialsError()
	}
	token, source := "", "authorization"
	if len(google) > 0 {
		_, _, native := nativegemini.Parse(r.Method, r.URL.Path)
		if !native || r.URL.RawPath != "" || len(google) != 1 || len(values) != 0 {
			return nil, sdkaccess.NewInvalidCredentialError()
		}
		token, source = strings.TrimSpace(google[0]), "x-goog-api-key"
	} else {
		if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
			return nil, sdkaccess.NewInvalidCredentialError()
		}
		token = strings.TrimSpace(strings.TrimPrefix(values[0], "Bearer "))
	}
	if token == "" || len(token) > 4096 {
		return nil, sdkaccess.NewInvalidCredentialError()
	}
	digest := sha256.Sum256([]byte(token))
	id := "admin"
	if subtle.ConstantTimeCompare(digest[:], p.admin[:]) != 1 {
		candidate := p.state.Load().byHash[digest]
		if candidate == nil || !p.active(candidate) {
			return nil, sdkaccess.NewInvalidCredentialError()
		}
		id = candidate.entry.ID
	}
	// The SDK receives the public key ID, never the client's secret bearer token.
	return &sdkaccess.Result{Provider: ProviderID, Principal: id, Metadata: map[string]string{"source": source, "key_id": id}}, nil
}

func (p *Provider) active(candidate *policy) bool {
	return candidate != nil && (candidate.entry.Enabled == nil || *candidate.entry.Enabled) && (candidate.entry.ExpiresAt == nil || p.now().Unix() < *candidate.entry.ExpiresAt)
}

func (p *Provider) Authorize(id, model string) error {
	if id == "admin" {
		return nil
	}
	candidate := p.state.Load().byID[id]
	if !p.active(candidate) {
		return fmt.Errorf("CPA key is disabled, expired or revoked")
	}
	if len(candidate.patterns) == 0 && len(candidate.entry.Prefixes) == 0 {
		return nil
	}
	prefix, _, _ := strings.Cut(model, "/")
	for _, allowed := range candidate.entry.Prefixes {
		if prefix == allowed {
			return nil
		}
	}
	for _, allowed := range candidate.patterns {
		if allowed.MatchString(model) {
			return nil
		}
	}
	return fmt.Errorf("model is not permitted for this CPA key")
}

func (p *Provider) KeyID(principal string) string {
	if principal == "admin" {
		return principal
	}
	if p.active(p.state.Load().byID[principal]) {
		return principal
	}
	return ""
}

func (p *Provider) Count() int { return len(p.state.Load().byID) }

// Limits returns the existing per-key policy without retaining any bearer key.
// Admission must call it for each new request/turn, not cache a semaphore sized
// at startup, because the GUI can lower limits while calls are still in flight.
// Zero limits mean unlimited only for the reserved authenticated admin identity.
func (p *Provider) Limits(id string) (rpm, concurrent int, ok bool) {
	if id == "admin" {
		return 0, 0, true
	}
	candidate := p.state.Load().byID[id]
	if !p.active(candidate) {
		return 0, 0, false
	}
	return candidate.entry.RPM, candidate.entry.MaxConcurrent, true
}

// compileGlob follows Python fnmatchcase's whole-string, slash-insensitive
// wildcards, rather than Go path.Match's slash-sensitive filesystem semantics.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	if len(pattern) == 0 || len(pattern) > 1024 {
		return nil, fmt.Errorf("invalid issued-key model pattern")
	}
	var out strings.Builder
	out.WriteString("(?s)\\A")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			out.WriteString(".*")
		case '?':
			out.WriteByte('.')
		case '[':
			j := i + 1
			if j < len(pattern) && pattern[j] == '!' {
				j++
			}
			if j < len(pattern) && pattern[j] == ']' {
				j++
			}
			for j < len(pattern) && pattern[j] != ']' {
				j++
			}
			if j == len(pattern) {
				out.WriteString(`\[`)
				continue
			}
			class := pattern[i+1 : j]
			out.WriteByte('[')
			if strings.HasPrefix(class, "!") {
				out.WriteByte('^')
				class = class[1:]
			} else if strings.HasPrefix(class, "^") {
				out.WriteByte('\\')
			}
			out.WriteString(strings.ReplaceAll(class, `\`, `\\`))
			out.WriteByte(']')
			i = j
		default:
			out.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		}
	}
	out.WriteString("\\z")
	compiled, err := regexp.Compile(out.String())
	if err != nil {
		return nil, fmt.Errorf("invalid issued-key model pattern")
	}
	return compiled, nil
}
