package catalogsync

import "encoding/json"

// Publication is confined to authenticated private IPC. It contains reviewed
// public capabilities and CAS hashes, never credential material or user prompts.
type Publication struct {
	Exposed     json.RawMessage `json:"exposed"`
	Context     json.RawMessage `json:"context"`
	OldExposed  string          `json:"old_exposed_sha256"`
	OldContext  string          `json:"old_context_sha256"`
	Config      string          `json:"config_sha256"`
	Accounts    string          `json:"accounts_sha256"`
	OldModels   []string        `json:"old_models"`
	NewModels   []string        `json:"new_models"`
	Added       []string        `json:"added"`
	Removed     []string        `json:"removed"`
	Rejected    []Rejected      `json:"rejected"`
	MorePending bool            `json:"more_pending"`
	Changed     bool            `json:"changed"`
}
