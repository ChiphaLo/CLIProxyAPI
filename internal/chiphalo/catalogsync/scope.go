package catalogsync

import (
	"errors"
	"slices"
	"strings"
	"unicode"
)

// Scope limits an explicit catalog review to selected models of one account.
// A nil scope keeps the legacy full review behavior.
type Scope struct {
	AccountID string   `json:"account_id"`
	Models    []string `json:"models"`
}

// Validate rejects ambiguous or unbounded scoped review requests.
func (s *Scope) Validate() error {
	if s == nil {
		return nil
	}
	id := strings.TrimSpace(s.AccountID)
	if id == "" || id != s.AccountID || len(id) > 256 || strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return errors.New("invalid scoped catalog account")
	}
	if len(s.Models) == 0 || len(s.Models) > 16 {
		return errors.New("scoped catalog review requires 1-16 models")
	}
	seen := map[string]bool{}
	for _, name := range s.Models {
		if !Eligible(name) || seen[name] {
			return errors.New("invalid scoped catalog model")
		}
		seen[name] = true
	}
	return nil
}

func (s *Scope) allows(accountID, name string) bool {
	return s == nil || s.AccountID == accountID && slices.Contains(s.Models, name)
}
