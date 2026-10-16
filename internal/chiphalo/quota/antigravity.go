package quota

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
)

const AntigravityModelsURL = "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"

var publicModelName = regexp.MustCompile(`^[a-zA-Z0-9._/-]{1,200}$`)

type AntigravityModel struct {
	Model     string   `json:"model"`
	Remaining *float64 `json:"remaining_percent"`
	Reset     *int64   `json:"reset_at"`
}

type AntigravityAccount struct {
	ID      string             `json:"id"`
	Prefix  string             `json:"prefix"`
	Status  string             `json:"status"`
	Checked int64              `json:"checked_at"`
	Updated *int64             `json:"updated_at"`
	Models  []AntigravityModel `json:"models"`
}

// ProjectAntigravity drops experiments, prompts and credential fields. Missing
// official quota is unknown, never a fabricated zero or unlimited allowance.
func ProjectAntigravity(raw []byte, allowed map[string]bool) ([]AntigravityModel, error) {
	var response struct {
		Models map[string]struct {
			Quota *struct {
				Remaining *float64 `json:"remainingFraction"`
				Reset     string   `json:"resetTime"`
			} `json:"quotaInfo"`
		} `json:"models"`
	}
	if len(raw) > 8<<20 || json.Unmarshal(raw, &response) != nil || response.Models == nil {
		return nil, errors.New("official antigravity quota schema unavailable")
	}
	rows := []AntigravityModel{}
	for name, model := range response.Models {
		if !allowed[name] || !publicModelName.MatchString(name) {
			continue
		}
		row := AntigravityModel{Model: name}
		if model.Quota != nil {
			if v := model.Quota.Remaining; v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0 && *v <= 1 {
				row.Remaining = new(*v * 100)
			}
			if stamp, err := time.Parse(time.RFC3339, model.Quota.Reset); err == nil && stamp.Unix() > 0 {
				row.Reset = new(stamp.Unix())
			}
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b AntigravityModel) int {
		if a.Model < b.Model {
			return -1
		}
		if a.Model > b.Model {
			return 1
		}
		return 0
	})
	if len(rows) > 256 {
		return nil, errors.New("official antigravity quota catalog exceeds bound")
	}
	return rows, nil
}

// FetchAntigravity performs only the fixed official metadata read. OAuth refresh
// remains exclusively owned by the SDK; GUI and collector never read token files.
func (f Fetcher) FetchAntigravity(ctx context.Context, credential Credential, allowed map[string]bool) ([]AntigravityModel, error) {
	if credential.Token == "" || len(credential.Token) > 65536 || credential.Workspace == "" || len(credential.Workspace) > 512 {
		return nil, errors.New("official antigravity credential unavailable")
	}
	factory := f.Transport
	if factory == nil {
		factory = defaultTransport
	}
	transport, err := factory(credential.Proxy)
	if err != nil {
		return nil, errors.New("official antigravity transport unavailable")
	}
	if idle, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer idle.CloseIdleConnections()
	}
	body, _ := json.Marshal(map[string]string{"project": credential.Workspace})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AntigravityModelsURL, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("official antigravity request unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+credential.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", misc.AntigravityUserAgent())
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("official antigravity transport unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("official_antigravity_http_%d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil {
		return nil, errors.New("official antigravity response unavailable")
	}
	return ProjectAntigravity(raw, allowed)
}

// PublicAntigravity validates cached projections again at every trust boundary.
func PublicAntigravity(rows []AntigravityAccount) ([]AntigravityAccount, error) {
	if len(rows) > 16 {
		return nil, errors.New("antigravity accounts exceed bound")
	}
	result := make([]AntigravityAccount, 0, len(rows))
	ids := map[string]bool{}
	for _, row := range rows {
		_, idErr := hex.DecodeString(row.ID)
		if idErr != nil || len(row.ID) != 64 || ids[row.ID] || !publicModelName.MatchString(row.Prefix) || len(row.Prefix) > 80 || row.Checked <= 0 || row.Status != "ok" && row.Status != "unavailable" || row.Status == "ok" && (row.Updated == nil || *row.Updated <= 0) || len(row.Models) > 256 {
			return nil, errors.New("antigravity projection invalid")
		}
		ids[row.ID] = true
		for _, model := range row.Models {
			if !publicModelName.MatchString(model.Model) || model.Remaining != nil && (math.IsNaN(*model.Remaining) || math.IsInf(*model.Remaining, 0) || *model.Remaining < 0 || *model.Remaining > 100) || model.Reset != nil && *model.Reset <= 0 {
				return nil, errors.New("antigravity model quota invalid")
			}
		}
		result = append(result, row)
	}
	return result, nil
}
