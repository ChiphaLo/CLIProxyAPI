package catalogsync

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// AccountSetting controls publication only. It never changes an OAuth credential,
// member binding, routing prefix, official capability, or quota/reset state.
type AccountSetting struct {
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	DisabledModels []string `json:"disabled_models"`
}

func ValidateAccountSetting(value AccountSetting, inventory []string) error {
	if !utf8.ValidString(value.Name) || strings.TrimSpace(value.Name) == "" || len([]rune(value.Name)) > 80 || strings.ContainsAny(value.Name, "\x00\r\n") || value.DisabledModels == nil || len(value.DisabledModels) > 512 {
		return errors.New("invalid Codex account settings")
	}
	seen := map[string]bool{}
	for _, name := range value.DisabledModels {
		found := false
		for _, available := range inventory {
			if available == name {
				found = true
				break
			}
		}
		if !found || seen[name] {
			return errors.New("model is not in this account's reviewed catalog")
		}
		seen[name] = true
	}
	return nil
}

// SelectedAccountModels preserves the reviewed inventory so disabled models can
// be re-enabled without an inference probe or borrowing another account's list.
func SelectedAccountModels(inventory map[string][]string, settings map[string]AccountSetting) map[string][]string {
	result := map[string][]string{}
	for id, names := range inventory {
		value, custom := settings[id]
		disabled := map[string]bool{}
		for _, name := range value.DisabledModels {
			disabled[name] = true
		}
		result[id] = []string{}
		for _, name := range names {
			if (!custom || value.Enabled) && !disabled[name] {
				result[id] = append(result[id], name)
			}
		}
	}
	return result
}
