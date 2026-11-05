package workbuddycli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"gopkg.in/yaml.v3"
)

// This opt-in, read-only acceptance guard is run before the existing trusted
// transaction helper. It proves that only the intended provider would change,
// without publishing unrelated GUI drafts or exposing configuration secrets.
func TestLivePublishedBundleScope(t *testing.T) {
	statePath := os.Getenv("CPA_WORKBUDDY_BUNDLE_STATE")
	if statePath == "" {
		t.Skip("explicit production-local read-only transaction guard only")
	}
	dir := "/opt/unified-ai-gateway/config"
	read := func(path string) []byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("cannot read local bundle input")
		}
		return raw
	}
	in := configbundle.Input{Config: read(filepath.Join(dir, "cliproxyapi.yaml")), Generated: read(filepath.Join(dir, "generated-models.yaml")), Context: read(filepath.Join(dir, "context-windows.json")), Accounts: read(filepath.Join(dir, "oauth-accounts.json")), Vault: read("/var/lib/cpa-admin/provider-secrets.json"), State: read(statePath), Now: time.Now()}
	out, err := configbundle.Build(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(raw []byte, isJSON bool) map[string]any {
		t.Helper()
		var value map[string]any
		var err error
		if isJSON {
			err = json.Unmarshal(raw, &value)
		} else {
			err = yaml.Unmarshal(raw, &value)
		}
		if err != nil {
			t.Fatal("cannot decode local bundle")
		}
		return value
	}
	strip := func(value map[string]any, field string, target func(map[string]any) bool) {
		rows, _ := value[field].([]any)
		clean := []any{}
		for _, row := range rows {
			entry, _ := row.(map[string]any)
			if !target(entry) {
				clean = append(clean, row)
			}
		}
		value[field] = clean
	}
	old, next := decode(in.Config, false), decode(out.Config, false)
	for _, value := range []map[string]any{old, next} {
		strip(value, "openai-compatibility", func(row map[string]any) bool { return row["name"] == "cpa-gui-workbuddy_intl-chat" })
	}
	if !reflect.DeepEqual(old, next) {
		t.Fatal("refusing transaction: another published provider or configuration field would change")
	}
	old, next = decode(in.Generated, false), decode(out.Generated, false)
	for _, value := range []map[string]any{old, next} {
		delete(value, "generated_at")
		for _, field := range []string{"chat_models", "responses_models"} {
			strip(value, field, func(row map[string]any) bool {
				id, _ := row["id"].(string)
				return strings.HasPrefix(id, "workbuddy-intl/")
			})
		}
	}
	if !reflect.DeepEqual(old, next) {
		t.Fatal("refusing transaction: another published model catalog entry would change")
	}
	old, next = decode(in.Context, true), decode(out.Context, true)
	for _, value := range []map[string]any{old, next} {
		models, _ := value["models"].(map[string]any)
		for id := range models {
			if strings.HasPrefix(id, "workbuddy-intl/") {
				delete(models, id)
			}
		}
	}
	if !reflect.DeepEqual(old, next) {
		t.Fatal("refusing transaction: another published context or pricing metadata entry would change")
	}
	t.Log("scope verified: published configuration, catalog and context outside WorkBuddy Intl are unchanged")
}
