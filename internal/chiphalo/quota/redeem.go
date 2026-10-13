package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var creditIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)
var redeemIdentifier = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)

type Redemption struct {
	OK        bool   `json:"ok"`
	Uncertain bool   `json:"uncertain,omitempty"`
	Error     string `json:"error,omitempty"`
	Code      string `json:"code,omitempty"`
	Status    string `json:"status,omitempty"`
	Windows   *int   `json:"windows_reset,omitempty"`
}

func creditExpiry(value any) (time.Time, bool) {
	if value == nil || value == "" {
		return time.Time{}, true
	}
	if text, ok := value.(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed, true
		}
		var number json.Number
		if json.Unmarshal([]byte(text), &number) == nil {
			if v, err := number.Float64(); err == nil {
				value = v
			}
		}
	}
	if v, ok := number(value); ok && v >= 0 && v <= 1e15 {
		if v > 100000000000 {
			v /= 1000
		}
		return time.Unix(int64(v), 0), true
	}
	return time.Time{}, false
}

// RedeemResetCredit is a single explicit, irreversible attempt. Callers must
// durably reserve its account/credit/request identity before calling it. GET
// metadata reads never call this function. No refresh, redirect or retry occurs.
// This operation is deliberately excluded from executable migration tests.
func (f Fetcher) RedeemResetCredit(ctx context.Context, credential Credential, creditID, requestID string, now time.Time, beforeSubmit func() error) Redemption {
	fail := func(message string) Redemption { return Redemption{Error: message} }
	if !creditIdentifier.MatchString(creditID) || !redeemIdentifier.MatchString(requestID) {
		return fail("invalid_reset_request_identity")
	}
	current, err := f.FetchResetCredits(ctx, credential)
	if err != nil || current.Status != "ok" {
		return fail("fresh_reset_credit_lookup_unavailable")
	}
	var selected *ResetCredit
	for i := range current.Credits {
		if current.Credits[i].ID == creditID {
			if selected != nil {
				return fail("ambiguous_reset_credit_identity")
			}
			selected = &current.Credits[i]
		}
	}
	if selected == nil || !strings.EqualFold(selected.Status, "available") || selected.Type != "codex_rate_limits" {
		return fail("reset_credit_not_available")
	}
	expires, valid := creditExpiry(selected.Expires)
	if !valid {
		return fail("reset_credit_expiry_invalid")
	}
	if !expires.IsZero() && !expires.After(now) {
		return fail("reset_credit_expired")
	}
	if ctx.Err() != nil {
		return fail("reset_request_canceled_before_submission")
	}
	if beforeSubmit == nil || beforeSubmit() != nil {
		return fail("fixed_reset_member_changed_before_submission")
	}
	factory := f.Transport
	if factory == nil {
		factory = defaultTransport
	}
	transport, err := factory(credential.Proxy)
	if err != nil || transport == nil {
		return fail("official_transport_unavailable")
	}
	if idle, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer idle.CloseIdleConnections()
	}
	raw, _ := json.Marshal(map[string]string{"credit_id": creditID, "redeem_request_id": requestID})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, OfficialResetCreditsURL+"/consume", bytes.NewReader(raw))
	if err != nil {
		return fail("reset_request_unavailable")
	}
	r.Header.Set("Authorization", "Bearer "+credential.Token)
	r.Header.Set("ChatGPT-Account-Id", credential.Workspace)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", "codex_cli_rs/0.76.0")
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("reset redirects forbidden") }}
	response, err := client.Do(r)
	uncertain := Redemption{Uncertain: true, Error: "reset_result_uncertain_refresh_official_status_do_not_retry"}
	if err != nil {
		return uncertain
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 262145))
	if err != nil || len(body) > 262144 {
		return uncertain
	}
	var result struct {
		Code   string `json:"code"`
		Credit struct {
			Status string `json:"status"`
		} `json:"credit"`
		Windows *int `json:"windows_reset"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &result) != nil {
		return uncertain
	}
	if result.Code != "reset" && !strings.EqualFold(result.Credit.Status, "redeemed") {
		return uncertain
	}
	if result.Windows != nil && (*result.Windows < 0 || *result.Windows > 10) {
		result.Windows = nil
	}
	return Redemption{OK: true, Code: "reset", Status: "redeemed", Windows: result.Windows}
}
