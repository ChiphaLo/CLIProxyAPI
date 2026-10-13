package adminhelper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/clientkeys"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatecontrol"
)

type HTTPBackend struct {
	origin, env string
	client      *http.Client
	private     bool
}

func NewUnixBackend(path string, uid uint32) *HTTPBackend {
	return &HTTPBackend{origin: "http://cpa-control", client: privatecontrol.Client(path, uid), private: true}
}

func NewHTTPBackend(origin, env string) (*HTTPBackend, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return nil, errors.New("explicit loopback candidate origin required")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1024 || port > 65535 {
		return nil, errors.New("unprivileged candidate port required")
	}
	return &HTTPBackend{origin: origin, env: env, client: &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("local candidate redirects are forbidden")
	}}}, nil
}
func (b *HTTPBackend) get(ctx context.Context, path string, authorized bool, target any) error {
	// This is a bounded local control-plane read, not an upstream provider call.
	if path != "/reset-credits" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, b.origin+path, nil)
	if err != nil {
		return errors.New("local candidate request unavailable")
	}
	if authorized && !b.private {
		key, err := clientkeys.ReadLegacyAdminKey(b.env)
		if err != nil {
			return errors.New("local candidate key unavailable")
		}
		r.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := b.client.Do(r)
	if err != nil {
		return errors.New("local candidate unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 && !(path == "/__cpa_release/ready" && resp.StatusCode == 503) {
		return errors.New("local candidate returned an unexpected status")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(raw) > 1024*1024 || json.Unmarshal(raw, target) != nil {
		return errors.New("local candidate returned an invalid bounded response")
	}
	return nil
}

func (b *HTTPBackend) Read(ctx context.Context, path string) (Result, error) {
	if !b.private || path != "/seed" && path != "/accounts" && path != "/__cpa_release/ready" && path != "/reset-credits" {
		return nil, errors.New("private inventory unavailable")
	}
	var value Result
	if err := b.get(ctx, path, false, &value); err != nil {
		return nil, err
	}
	return value, nil
}
func (b *HTTPBackend) Catalog(ctx context.Context) ([]string, error) {
	var value struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := b.get(ctx, "/v1/models", true, &value); err != nil {
		return nil, err
	}
	if value.Data == nil || len(value.Data) > 5000 {
		return nil, errors.New("local candidate catalog invalid")
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, row := range value.Data {
		if row.ID == "" || seen[row.ID] {
			return nil, errors.New("local candidate catalog invalid")
		}
		seen[row.ID] = true
		ids = append(ids, row.ID)
	}
	slices.Sort(ids)
	return ids, nil
}
func (b *HTTPBackend) Await(ctx context.Context, digest string, models []string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var ready struct {
			Ready              bool   `json:"ready"`
			ConfigurationReady bool   `json:"configuration_ready"`
			ProductionReady    bool   `json:"production_ready"`
			StateContract      string `json:"state_contract"`
			Digest             string `json:"config_sha256"`
			Catalog            struct {
				Ready bool `json:"ready"`
			} `json:"catalog"`
		}
		if b.get(ctx, "/__cpa_release/ready", false, &ready) == nil && ready.StateContract == "cpa-shadow-validation-v1" && !ready.ProductionReady && ready.Digest == digest && ready.Catalog.Ready && (ready.Ready || ready.ConfigurationReady) {
			ids, err := b.Catalog(ctx)
			if err == nil && slices.Equal(ids, models) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("actual candidate catalog readiness was not confirmed")
		case <-ticker.C:
		}
	}
}
