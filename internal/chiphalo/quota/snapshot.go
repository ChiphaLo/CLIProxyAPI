package quota

import (
	"context"
	"encoding/json"
	"time"
)

type Account struct {
	ID          string `json:"id"`
	Prefix      string `json:"prefix"`
	Label       string `json:"label"`
	Workspace   string `json:"workspace_id"`
	LegacySince *int64 `json:"legacy_since"`
	Checked     int64  `json:"checked_at"`
	Updated     *int64 `json:"updated_at"`
	Source      string `json:"source"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	Quota       *Usage `json:"quota"`
}
type Snapshot struct {
	Accounts []Account `json:"accounts"`
	Checked  int64     `json:"checked_at"`
}

type Binding struct {
	ID, Prefix, Label, Workspace string
	LegacySince                  *int64
}
type Lookup func(context.Context, Binding) (Credential, error)

// Collect has no file access and never refreshes OAuth material. The lookup is
// provided by the gateway's validated account registry and live SDK manager.
func Collect(ctx context.Context, bindings []Binding, lookup Lookup, fetcher Fetcher, clock func() time.Time) Snapshot {
	if clock == nil {
		clock = time.Now
	}
	rows := make([]Account, len(bindings))
	jobs := make(chan int)
	done := make(chan struct{}, 2)
	for worker := 0; worker < 2; worker++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for index := range jobs {
				binding := bindings[index]
				row := Account{ID: binding.ID, Prefix: binding.Prefix, Label: binding.Label, Workspace: binding.Workspace, LegacySince: binding.LegacySince, Checked: clock().Unix(), Source: OfficialUsageURL, Status: "unavailable"}
				credential, err := lookup(ctx, binding)
				if err != nil {
					row.Error = "auth_mapping_unavailable"
				} else {
					value, err := fetcher.Fetch(ctx, credential)
					if err != nil {
						row.Error = err.Error()
					} else {
						row.Status = "ok"
						row.Quota = &value
						row.Updated = new(clock().Unix())
					}
				}
				rows[index] = row
			}
		}()
	}
	for index := range bindings {
		jobs <- index
	}
	close(jobs)
	<-done
	<-done
	return Snapshot{Accounts: rows, Checked: clock().Unix()}
}

// CarryPrevious keeps stale quota only for the same verified public account
// tuple. Collector failure must not turn an unavailable account into zero usage.
func CarryPrevious(current Snapshot, previous Snapshot) Snapshot {
	byID := map[string]Account{}
	for _, row := range previous.Accounts {
		byID[row.ID] = row
	}
	for i, row := range current.Accounts {
		if row.Status == "ok" {
			continue
		}
		old, ok := byID[row.ID]
		if !ok || row.Prefix != old.Prefix || row.Workspace != old.Workspace {
			continue
		}
		if (row.LegacySince == nil) != (old.LegacySince == nil) || row.LegacySince != nil && *row.LegacySince != *old.LegacySince {
			continue
		}
		current.Accounts[i].Quota = old.Quota
		current.Accounts[i].Updated = old.Updated
	}
	return current
}

// DecodeSnapshot accepts only the non-secret projection schema, so unexpected
// original fields are dropped when an old public snapshot is republished.
func DecodeSnapshot(raw []byte) (Snapshot, error) {
	var value Snapshot
	err := json.Unmarshal(raw, &value)
	if err != nil {
		return value, err
	}
	for i, row := range value.Accounts {
		if row.Quota != nil {
			raw, _ := json.Marshal(row.Quota)
			projected, err := Project(raw)
			if err != nil {
				return Snapshot{}, err
			}
			value.Accounts[i].Quota = &projected
		}
	}
	return value, err
}
