// Package release implements the single-writer blue/green routing transaction.
// Shared auth, receipts and quota state are deliberately outside the transaction.
package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatecontrol"
)

const StateContract = "cpa-legacy-v1"

var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)

// Manifest contains no keys or auth material. Each release has a private,
// immutable directory containing manifest.json and its hashed artifacts.
type Manifest struct {
	ID                   string            `json:"release_id"`
	Endpoint             string            `json:"endpoint"`
	StateContract        string            `json:"state_contract"`
	ConfigSHA256         string            `json:"config_sha256"`
	Artifacts            map[string]string `json:"artifacts"`
	SharedBusinessConfig bool              `json:"shared_business_config,omitempty"`
	PrivateControlSocket string            `json:"private_control_socket,omitempty"`
	GatewayUID           uint32            `json:"gateway_uid,omitempty"`
}

type Reference struct {
	ID     string `json:"release_id"`
	Digest string `json:"manifest_sha256"`
}

type State struct {
	Revision uint64    `json:"revision"`
	Current  Reference `json:"current"`
	Previous Reference `json:"previous"`
}

type selection struct {
	state    State
	manifest Manifest
	handler  http.Handler
}

type Controller struct {
	root           string
	mu             sync.Mutex
	active         atomic.Pointer[selection]
	lock           *os.File
	client         *http.Client
	proxyTransport *http.Transport
	closed         bool
	ingress        ingressPolicy
}

// Open requires a dedicated, private release root. It refuses concurrent
// controllers; the OS lock is held for the entire process lifetime.
func Open(root string) (*Controller, error) {
	return OpenWithTrustedIngress(root, nil)
}

// OpenWithTrustedIngress accepts forwarding metadata only from explicitly
// configured loopback peers. The policy is fixed for the controller lifetime.
// Without this opt-in all client-supplied forwarding headers remain untrusted.
func OpenWithTrustedIngress(root string, peers []string) (*Controller, error) {
	ingress, err := parseIngress(peers)
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("release root must be an existing real directory")
	}
	if err := requirePrivate(info); err != nil {
		return nil, err
	}
	lock, err := lockRoot(absolute)
	if err != nil {
		return nil, err
	}
	c := &Controller{root: absolute, lock: lock, ingress: ingress, proxyTransport: &http.Transport{Proxy: nil}, client: &http.Client{
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
	raw, err := readRegular(filepath.Join(absolute, "current.json"), 64*1024)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	var state State
	if err := decode(raw, &state); err != nil || state.Revision == 0 {
		_ = c.Close()
		return nil, fmt.Errorf("invalid durable release state")
	}
	s, err := c.load(state.Current.ID)
	if err != nil || s.state.Current != state.Current {
		_ = c.Close()
		return nil, fmt.Errorf("active release artifacts differ from committed state")
	}
	s.state = state
	c.active.Store(s)
	return c, nil
}

func (c *Controller) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.client.CloseIdleConnections()
	c.proxyTransport.CloseIdleConnections()
	return c.lock.Close()
}

func (c *Controller) State() State {
	if current := c.active.Load(); current != nil {
		return current.state
	}
	return State{}
}

func (c *Controller) load(id string) (*selection, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("invalid release ID")
	}
	dir := filepath.Join(c.root, "releases", id)
	for _, path := range []string{filepath.Join(c.root, "releases"), dir} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("release directory must not be a symlink")
		}
		if err := requirePrivate(info); err != nil {
			return nil, err
		}
	}
	raw, err := readRegular(filepath.Join(dir, "manifest.json"), 64*1024)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := decode(raw, &manifest); err != nil {
		return nil, fmt.Errorf("invalid release manifest")
	}
	if manifest.ID != id || manifest.StateContract != StateContract || (!manifest.SharedBusinessConfig && !validDigest(manifest.ConfigSHA256)) || (manifest.SharedBusinessConfig && manifest.ConfigSHA256 != "") {
		return nil, fmt.Errorf("release identity or shared data contract is incompatible")
	}
	if manifest.SharedBusinessConfig && (!filepath.IsAbs(manifest.PrivateControlSocket) || filepath.Clean(manifest.PrivateControlSocket) != manifest.PrivateControlSocket || manifest.GatewayUID == 0) {
		return nil, fmt.Errorf("shared business release requires a fixed private gateway identity")
	}
	endpoint, err := loopbackURL(manifest.Endpoint)
	if err != nil {
		return nil, err
	}
	if len(manifest.Artifacts) < 2 || len(manifest.Artifacts) > 64 || (!manifest.SharedBusinessConfig && manifest.Artifacts["config.yaml"] != manifest.ConfigSHA256) || (manifest.SharedBusinessConfig && (manifest.Artifacts["config.yaml"] != "" || manifest.Artifacts["hosting.json"] == "")) {
		return nil, fmt.Errorf("release must pin its config and executable artifacts")
	}
	for name, expected := range manifest.Artifacts {
		if name == "manifest.json" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\:`) || !validDigest(expected) {
			return nil, fmt.Errorf("invalid release artifact")
		}
		file, err := openRegular(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		n, errHash := io.Copy(hash, io.LimitReader(file, 512*1024*1024+1))
		errClose := file.Close()
		if errHash != nil || errClose != nil || n > 512*1024*1024 || hex.EncodeToString(hash.Sum(nil)) != expected {
			return nil, fmt.Errorf("release artifact digest mismatch")
		}
	}
	digest := sha256.Sum256(raw)
	return &selection{manifest: manifest, state: State{Current: Reference{ID: id, Digest: hex.EncodeToString(digest[:])}}, handler: newProxy(endpoint, c.proxyTransport, c.ingress)}, nil
}

// Activate commits exactly one coherent route/config identity. An old request
// retains its selection until completion; no old process is stopped here.
func (c *Controller) Activate(ctx context.Context, id string, expectedRevision uint64) (State, error) {
	return c.change(ctx, id, expectedRevision, false)
}

// Rollback reselects the pinned previous release; it never restores data files.
func (c *Controller) Rollback(ctx context.Context, expectedRevision uint64) (State, error) {
	return c.change(ctx, "", expectedRevision, true)
}

func (c *Controller) change(ctx context.Context, id string, expectedRevision uint64, rollback bool) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.State(), fmt.Errorf("release controller is closed")
	}
	before := c.State()
	if before.Revision != expectedRevision {
		return before, fmt.Errorf("release revision conflict")
	}
	if rollback {
		id = before.Previous.ID
	}
	candidate, err := c.load(id)
	if err != nil {
		return before, err
	}
	if rollback && candidate.state.Current != before.Previous {
		return before, fmt.Errorf("rollback release changed since activation")
	}
	if candidate.state.Current == before.Current {
		return before, nil
	}
	if err := c.ready(ctx, candidate.manifest); err != nil {
		return before, err
	}
	if err := ctx.Err(); err != nil {
		return before, err
	}
	next := State{Revision: before.Revision + 1, Current: candidate.state.Current, Previous: before.Current}
	if next.Revision == 0 {
		return before, fmt.Errorf("release revision overflow")
	}
	data, err := json.Marshal(next)
	if err != nil {
		return before, err
	}
	renamed, err := commitFile(c.root, append(data, '\n'))
	if !renamed {
		return before, err
	}
	// Once rename succeeds we must align live routing with the committed file,
	// even if the directory fsync reports a durability error. The caller receives
	// the new state and an explicit error rather than falsely claiming rollback.
	candidate.state = next
	c.active.Store(candidate)
	return next, err
}

func (c *Controller) ready(ctx context.Context, manifest Manifest) error {
	client, endpoint := c.client, manifest.Endpoint
	if manifest.SharedBusinessConfig {
		client = privatecontrol.Client(manifest.PrivateControlSocket, manifest.GatewayUID)
		defer client.CloseIdleConnections()
		endpoint = "http://cpa-control"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/__cpa_release/ready", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("candidate is not reachable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("candidate is not ready")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(raw) > 8192 {
		return fmt.Errorf("invalid candidate readiness reply")
	}
	var ready struct {
		ID                 string          `json:"release_id"`
		ConfigSHA256       string          `json:"config_sha256"`
		StateContract      string          `json:"state_contract"`
		Ready              bool            `json:"ready"`
		ProductionReady    bool            `json:"production_ready,omitempty"`
		ConfigurationReady bool            `json:"configuration_ready,omitempty"`
		Pending            bool            `json:"config_apply_pending,omitempty"`
		Refresh            bool            `json:"credential_refresh,omitempty"`
		Catalog            json.RawMessage `json:"catalog,omitempty"`
	}
	if decode(raw, &ready) != nil || !ready.Ready || ready.ID != manifest.ID || ready.StateContract != manifest.StateContract {
		return fmt.Errorf("candidate readiness identity mismatch")
	}
	if manifest.SharedBusinessConfig {
		if !ready.ProductionReady || !ready.ConfigurationReady || ready.Pending || !validDigest(ready.ConfigSHA256) {
			return fmt.Errorf("shared business configuration readiness is unconfirmed")
		}
	} else if ready.ConfigSHA256 != manifest.ConfigSHA256 {
		return fmt.Errorf("candidate configuration identity mismatch")
	}
	return nil
}

func loopbackURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("release endpoint must be explicit HTTP loopback with a port")
	}
	return u, nil
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

func decode(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

func openRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("release files must be regular, not symlinks")
	}
	if err := requirePrivate(info); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("release file changed while opening")
	}
	return file, nil
}

func readRegular(path string, limit int64) ([]byte, error) {
	file, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, fmt.Errorf("release metadata read limit")
	}
	return raw, nil
}
