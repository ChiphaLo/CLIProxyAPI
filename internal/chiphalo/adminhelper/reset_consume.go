package adminhelper

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/quota"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var resetInputID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

type resetIntent struct {
	Account string `json:"account_id"`
	Prefix  string `json:"prefix"`
	Credit  string `json:"credit_id"`
	Request string `json:"redeem_request_id"`
	Phase   string `json:"phase"`
}

func resetUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

// consumeReset schedules one explicitly requested action. An immutable ledger
// reservation survives process/release changes and prevents a timeout, double
// click or restart from automatically resubmitting an irreversible credit use.
// Shadow readiness refuses this action. No migration test invokes this path.
func (s *Service) consumeReset(ctx context.Context, payload map[string]json.RawMessage) Result {
	if allowed(payload, "prefix", "credit_id", "job_id") != nil {
		return failure("unknown reset action fields")
	}
	s.redeemMu.Lock()
	defer s.redeemMu.Unlock()
	if raw, ok := payload["job_id"]; ok {
		var id string
		if len(payload) != 1 || json.Unmarshal(raw, &id) != nil || !requestIdentity.MatchString(id) {
			return failure("invalid reset job identity")
		}
		if s.redeemJob == nil || s.redeemJob.id != id {
			return failure("reset job unavailable; refresh official status, do not resubmit")
		}
		if s.redeemJob.pending {
			return Result{"ok": true, "pending": true, "job_id": id}
		}
		return s.redeemJob.result
	}
	if s.redeemJob != nil && s.redeemJob.pending {
		return failure("reset operation already in progress")
	}
	if s.inventory == nil || s.apply == nil {
		return failure("reset control unavailable")
	}
	prefix, e1 := text(payload, "prefix", 128)
	credit, e2 := text(payload, "credit_id", 200)
	if e1 != nil || e2 != nil || !resetInputID.MatchString(prefix) || !resetInputID.MatchString(credit) {
		return failure("invalid reset account or credit identity")
	}
	ready, err := s.inventory.Read(ctx, "/__cpa_release/ready")
	if err != nil || ready["production_ready"] != true || ready["state_contract"] != "cpa-legacy-v1" || ready["ready"] != true {
		return failure("reset use disabled outside verified production hosting")
	}
	rows, err := s.inventory.Read(ctx, "/accounts")
	if err != nil {
		return failure("fixed reset account inventory unavailable")
	}
	list, _ := rows["accounts"].([]any)
	account := ""
	for _, value := range list {
		row, _ := value.(map[string]any)
		if row["prefix"] == prefix && row["enabled"] == true {
			if account != "" {
				return failure("ambiguous reset account")
			}
			account, _ = row["id"].(string)
		}
	}
	if account == "" {
		return failure("fixed reset account unavailable")
	}
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return failure("reset action canceled before reservation")
	}
	f, err := openPrivate(s.paths.Lock)
	if err != nil {
		return failure("reset reservation lock unavailable")
	}
	defer func() { _ = f.Close() }()
	if lockContext(ctx, f) != nil {
		return failure("reset reservation lock unavailable")
	}
	defer unlock(f)
	key, _ := json.Marshal([]string{account, credit})
	path := filepath.Join(s.apply.options.Transactions, "reset-consume-"+hash(key)+".json")
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return Result{"ok": false, "uncertain": true, "error": "credit_use_previously_reserved_refresh_official_status_do_not_retry"}
	}
	request, err := resetUUID()
	if err != nil {
		return failure("reset request identity unavailable")
	}
	token, err := randomHex(16)
	if err != nil {
		return failure("reset job identity unavailable")
	}
	intent := resetIntent{Account: account, Prefix: prefix, Credit: credit, Request: request, Phase: "reserved"}
	raw, _ := json.Marshal(intent)
	if writePrivate(path, append(raw, '\n')) != nil {
		return Result{"ok": false, "uncertain": true, "error": "reset_reservation_durability_unconfirmed_no_submission"}
	}
	background, cancel := context.WithCancel(context.Background())
	job := &catalogJob{id: "redeem-" + token, pending: true, cancel: cancel}
	s.redeemJob = job
	s.redeemWorkers.Add(1)
	go func() {
		defer s.redeemWorkers.Done()
		defer cancel()
		result := Result{"ok": false, "uncertain": true, "error": "reset_result_uncertain_refresh_official_status_do_not_retry"}
		if s.audit(job.id, "consume_oauth_reset_credit", "job_started", false, 0) == nil {
			value, err := s.inventory.consumeReserved(background, intent)
			if err == nil {
				result = value
			}
		}
		intent.Phase = "uncertain"
		if result["ok"] == true {
			intent.Phase = "completed"
		}
		raw, _ := json.Marshal(intent)
		if writePrivate(path, append(raw, '\n')) != nil {
			result = Result{"ok": false, "uncertain": true, "error": "reset_completion_durability_unconfirmed_do_not_retry"}
		}
		if s.audit(job.id, "consume_oauth_reset_credit", "job_completed", result["ok"] == true, 0) != nil {
			result = Result{"ok": false, "uncertain": true, "error": "reset_completion_audit_unconfirmed_do_not_retry"}
		}
		result["pending"], result["job_id"] = false, job.id
		s.redeemMu.Lock()
		job.result = result
		job.pending = false
		s.redeemMu.Unlock()
	}()
	return Result{"ok": true, "pending": true, "job_id": job.id}
}
func (s *Service) stopResetJobs() {
	s.redeemMu.Lock()
	if s.redeemJob != nil {
		s.redeemJob.cancel()
	}
	s.redeemMu.Unlock()
	s.redeemWorkers.Wait()
}
func (b *HTTPBackend) consumeReserved(ctx context.Context, intent resetIntent) (Result, error) {
	if !b.private {
		return nil, errors.New("private reset action unavailable")
	}
	if strings.ContainsAny(intent.Account, "\r\n\x00") {
		return nil, errors.New("reset account invalid")
	}
	raw, _ := json.Marshal(map[string]string{"prefix": intent.Prefix, "credit_id": intent.Credit, "redeem_request_id": intent.Request, "expected_account_id": intent.Account})
	r, err := http.NewRequestWithContext(ctx, "POST", b.origin+"/consume-reset-credit", bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("reset request unavailable")
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(r)
	if err != nil {
		return nil, errors.New("reset result uncertain")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	var value quota.Redemption
	if err != nil || len(body) > 4096 || resp.StatusCode != 200 || json.Unmarshal(body, &value) != nil {
		return nil, errors.New("reset result uncertain")
	}
	projected, _ := json.Marshal(value)
	var result Result
	_ = json.Unmarshal(projected, &result)
	return result, nil
}
