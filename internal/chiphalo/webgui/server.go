package webgui

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/adminhelper"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/configbundle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatecontrol"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
)

//go:embed assets/index.html
var index []byte
var indexCompressed = []byte{}
var indexTag = ""

func init() {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	_, _ = writer.Write(index)
	_ = writer.Close()
	indexCompressed = buffer.Bytes()
	hash := sha256.Sum256(index)
	indexTag = "W/\"" + hex.EncodeToString(hash[:]) + "\""
}

type Helper interface {
	Call(context.Context, string, map[string]any) (map[string]any, error)
}
type UnixHelper struct {
	Path string
	UID  uint32
}

func (h UnixHelper) Call(ctx context.Context, op string, payload map[string]any) (map[string]any, error) {
	conn, err := privatecontrol.Dial(ctx, h.Path, h.UID)
	if err != nil {
		return nil, errors.New("helper unavailable")
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(map[string]any{"op": op, "request_id": "gui-" + hex.EncodeToString(nonce), "payload": payload})
	if err != nil || len(raw)+1 > adminhelper.MaxRequest {
		return nil, errors.New("helper request too large")
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return nil, errors.New("helper transport failed")
	}
	line, err := bufio.NewReader(io.LimitReader(conn, adminhelper.MaxResponse+1)).ReadBytes('\n')
	if err != nil || len(line) > adminhelper.MaxResponse {
		return nil, errors.New("incomplete helper response")
	}
	var result map[string]any
	if privatefile.Decode(line, &result) != nil || result == nil {
		return nil, errors.New("invalid helper response")
	}
	return result, nil
}

type Options struct {
	Data, Usage, Quota, Exposed, Origin string
	History                             string
	Helper                              Helper
	TrustedPeers                        []string
	Now                                 func() time.Time
	Resolve                             configbundle.Resolver
}
type Server struct {
	options    Options
	auth       authStore
	mu         sync.Mutex
	trusted    map[netip.Addr]bool
	slots      chan struct{}
	fitCache   quotaFitCache
	resetCache resetCreditCache
}

func New(options Options) (*Server, error) {
	if options.Helper == nil || options.Origin != "https://cpa.chipha.de" {
		return nil, errors.New("explicit CPA helper and HTTPS origin required")
	}
	for _, path := range []string{options.Data, options.Usage, options.Quota, options.Exposed} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, errors.New("absolute GUI paths required")
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path {
			return nil, errors.New("existing real GUI paths required")
		}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	s := &Server{options: options, trusted: map[netip.Addr]bool{}, slots: make(chan struct{}, 8)}
	s.auth = authStore{config: filepath.Join(options.Data, "gui.json"), sessions: filepath.Join(options.Data, "sessions.json"), now: options.Now, failures: map[string][]int64{}}
	s.fitCache.compute = s.computeQuotaFit
	if _, err := s.auth.load(); err != nil {
		return nil, err
	}
	if _, err := s.auth.sessionDoc(); err != nil {
		return nil, err
	}
	for _, ip := range options.TrustedPeers {
		peer, err := netip.ParseAddr(ip)
		if err != nil || !peer.IsLoopback() {
			return nil, errors.New("only explicit loopback GUI ingress peers supported")
		}
		s.trusted[peer.Unmap()] = true
	}
	return s, nil
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func reject(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]any{"ok": false, "error": message})
}
func (s *Server) ip(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	if s.trusted[peer.Unmap()] {
		candidate := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
		if ip, err := netip.ParseAddr(candidate); err == nil {
			return ip.Unmap().String()
		}
	}
	return peer.Unmap().String()
}
func (s *Server) call(ctx context.Context, op string, payload map[string]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	return s.options.Helper.Call(ctx, op, payload)
}
func (s *Server) forward(w http.ResponseWriter, r *http.Request, op string, payload map[string]any) {
	value, err := s.call(r.Context(), op, payload)
	if err != nil {
		reject(w, 503, "helper unavailable")
		return
	}
	status := 200
	if value["ok"] != true {
		status = 400
	}
	respond(w, status, value)
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		reject(w, 429, "GUI concurrency limit reached")
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	path := r.URL.Path
	if strings.HasPrefix(path, "/gui/") {
		path = strings.TrimPrefix(path, "/gui")
	}
	if (path == "/" || path == "/index.html") && r.Method == "GET" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("ETag", indexTag)
		if r.Header.Get("If-None-Match") == indexTag {
			w.WriteHeader(304)
			return
		}
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip;q=0") {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(indexCompressed)
		} else {
			_, _ = w.Write(index)
		}
		return
	}
	if !strings.HasPrefix(path, "/api/") {
		reject(w, 404, "not found")
		return
	}
	mutating := r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" || r.Method == "DELETE"
	if mutating {
		origin := r.Header.Get("Origin")
		if origin != "" && origin != s.options.Origin || path != "/api/login" && origin != s.options.Origin {
			reject(w, 403, "origin rejected")
			return
		}
	}
	var body map[string]any
	if mutating {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
		if err != nil {
			reject(w, 413, "request too large")
			return
		}
		if len(raw) > 0 && configbundle.DecodeDraft(raw, &body) != nil {
			reject(w, 400, "invalid JSON object")
			return
		}
		if body == nil {
			body = map[string]any{}
		}
	}
	cookie, _ := r.Cookie("cpa_session")
	token := ""
	if cookie != nil {
		token = cookie.Value
	}
	if path == "/api/login" && r.Method == "POST" {
		password, _ := body["password"].(string)
		value, err := s.auth.login(s.ip(r), password)
		if err != nil {
			status := 401
			if err.Error() == "too many attempts" {
				status = 429
			}
			reject(w, status, err.Error())
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "cpa_session", Value: value, Path: "/gui", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL)})
		respond(w, 200, map[string]any{"ok": true, "csrf": s.auth.csrf(value)})
		return
	}
	if !s.auth.verify(token) {
		reject(w, 401, "unauthorized")
		return
	}
	if mutating && !hmacToken(r.Header.Get("X-CSRF-Token"), s.auth.csrf(token)) {
		reject(w, 403, "csrf rejected")
		return
	}
	switch {
	case path == "/api/logout" && r.Method == "POST":
		if s.auth.logout(token) != nil {
			reject(w, 503, "session store unavailable")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "cpa_session", Path: "/gui", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
		respond(w, 200, map[string]any{"ok": true})
	case path == "/api/admin/csrf" && r.Method == "GET":
		respond(w, 200, map[string]any{"csrf": s.auth.csrf(token)})
	case path == "/api/admin/password" && r.Method == "POST":
		old, _ := body["old_password"].(string)
		next, _ := body["new_password"].(string)
		if err := s.auth.changePassword(old, next); err != nil {
			reject(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]any{"ok": true})
	case path == "/api/keys" && r.Method == "GET":
		s.forward(w, r, "read_api_keys", nil)
	case path == "/api/keys" && r.Method == "POST":
		s.forward(w, r, "create_api_key", body)
	case strings.HasPrefix(path, "/api/keys/") && (r.Method == "PATCH" || r.Method == "DELETE"):
		id := strings.TrimPrefix(path, "/api/keys/")
		if id == "" || strings.Contains(id, "/") {
			reject(w, 404, "not found")
			return
		}
		body["key_id"] = id
		op := "update_api_key"
		if r.Method == "DELETE" {
			op = "delete_api_key"
		}
		s.forward(w, r, op, body)
	case (path == "/api/model" || path == "/api/models") && r.Method == "GET":
		s.forward(w, r, "gateway_models", nil)
	case path == "/api/state" && r.Method == "GET":
		s.stateResponse(w, r)
	case path == "/api/status" && r.Method == "GET":
		s.statusResponse(w, r)
	case path == "/api/apply" && r.Method == "POST":
		s.apply(w, r)
	case path == "/api/providers" || strings.HasPrefix(path, "/api/providers/"):
		s.providers(w, r, path, body)
	case path == "/api/prices":
		s.pricesRoute(w, r, body)
	case path == "/api/usage" && r.Method == "GET":
		s.usageResponse(w, r)
	case path == "/api/oauth-accounts" && r.Method == "GET":
		s.accountResponse(w, r)
	case path == "/api/quota-model" && r.Method == "GET":
		window := "recent"
		if r.URL.Query().Get("window") == "all" {
			window = "all"
		}
		cycles := int(max(1, min(integerQuery(r.URL.Query(), "cycles", 6), 24)))
		respond(w, 200, s.quotaFitReport(window, cycles, r.URL.Query().Get("fresh") == "1"))
	default:
		reject(w, 503, "Go migration for this operation is not complete")
	}
}
func hmacToken(a, b string) bool {
	return a != "" && b != "" && hmac.Equal([]byte(a), []byte(b))
}
