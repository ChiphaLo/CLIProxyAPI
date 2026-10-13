// Package webgui ports the existing GUI control surface without exposing gateway credentials.
package webgui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/privatefile"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/pbkdf2"
)

const sessionTTL int64 = 12 * 3600
const idleTTL int64 = 30 * 60

type authStore struct {
	mu               sync.Mutex
	config, sessions string
	now              func() time.Time
	failures         map[string][]int64
}
type session struct {
	Exp     int64 `json:"exp"`
	Epoch   int64 `json:"epoch"`
	Created int64 `json:"created_at"`
	Last    int64 `json:"last_seen"`
}
type authConfig struct {
	Admin  struct{ Scheme, Hash, Salt string }
	Secret string `json:"secret"`
	Epoch  int64  `json:"session_epoch"`
}

func readJSON(path string, limit int64, secret bool, target any) error {
	raw, err := privatefile.Read(path, limit, secret)
	if err != nil {
		return errors.New("GUI data unavailable")
	}
	return privatefile.Decode(raw, target)
}
func writeJSON(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("existing GUI file required")
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	limit := 2 * 1024 * 1024
	if filepath.Base(path) == "state.json" {
		limit = 4 * 1024 * 1024
	}
	if len(raw) > limit {
		return errors.New("GUI document exceeds management limit")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".gui-*")
	if err != nil {
		return errors.New("cannot stage GUI data")
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err = f.Chmod(info.Mode().Perm()); err == nil {
		err = preserveFileOwner(f, info)
	}
	if err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		return errors.New("cannot persist GUI data")
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return errors.New("GUI data changed during write")
	}
	if os.Rename(f.Name(), path) != nil {
		return errors.New("cannot publish GUI data")
	}
	if syncParent(filepath.Dir(path)) != nil {
		return errors.New("GUI durability unconfirmed")
	}
	return nil
}
func (a *authStore) load() (authConfig, error) {
	var c authConfig
	err := readJSON(a.config, 2*1024*1024, true, &c)
	if err != nil || c.Secret == "" || len(c.Secret) > 512 || c.Admin.Hash == "" {
		return c, errors.New("GUI authentication data invalid")
	}
	if c.Epoch == 0 {
		c.Epoch = 1
	}
	return c, nil
}
func verifyPassword(c authConfig, password string) bool {
	if len(password) == 0 || len(password) > 4096 {
		return false
	}
	if c.Admin.Scheme == "argon2id" {
		parts := strings.Split(c.Admin.Hash, "$")
		if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
			return false
		}
		var memory, iterations, parallel uint32
		if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallel); err != nil || n != 3 || memory < 8192 || memory > 131072 || iterations < 1 || iterations > 8 || parallel < 1 || parallel > 4 {
			return false
		}
		salt, e1 := base64.RawStdEncoding.DecodeString(parts[4])
		expected, e2 := base64.RawStdEncoding.DecodeString(parts[5])
		if e1 != nil || e2 != nil || len(salt) < 8 || len(salt) > 64 || len(expected) < 16 || len(expected) > 64 {
			return false
		}
		actual := argon2.IDKey([]byte(password), salt, iterations, memory, uint8(parallel), uint32(len(expected)))
		return subtle.ConstantTimeCompare(actual, expected) == 1
	}
	if c.Admin.Scheme != "" || c.Admin.Salt == "" || len(c.Admin.Salt) > 256 {
		return false
	}
	actual := pbkdf2.Key([]byte(password), []byte(c.Admin.Salt), 200000, 32, sha256.New)
	expected, err := hex.DecodeString(c.Admin.Hash)
	return err == nil && subtle.ConstantTimeCompare(actual, expected) == 1
}
func passwordHash(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	value := argon2.IDKey([]byte(password), salt, 3, 65536, 1, 32)
	return "$argon2id$v=19$m=65536,t=3,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(value), nil
}
func sessionID(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])[:32]
}
func signature(secret, text string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(text))
	return hex.EncodeToString(mac.Sum(nil))
}
func (a *authStore) csrf(token string) string {
	c, err := a.load()
	if err != nil {
		return ""
	}
	return signature(c.Secret, token)[:32]
}
func (a *authStore) sessionDoc() (map[string]session, error) {
	var values map[string]session
	if err := readJSON(a.sessions, 2*1024*1024, true, &values); err != nil {
		return nil, err
	}
	if values == nil || len(values) > 8192 {
		return nil, errors.New("invalid GUI session database")
	}
	return values, nil
}
func (a *authStore) prune(values map[string]session) {
	now := a.now().Unix()
	for id, value := range values {
		last := value.Last
		if last == 0 {
			last = value.Created
		}
		if value.Exp <= now || last <= 0 || now-last > idleTTL {
			delete(values, id)
		}
	}
}
func (a *authStore) login(ip, password string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now().Unix()
	for key, times := range a.failures {
		out := times[:0]
		for _, stamp := range times {
			if stamp > now-300 {
				out = append(out, stamp)
			}
		}
		if len(out) == 0 {
			delete(a.failures, key)
		} else {
			a.failures[key] = out
		}
	}
	if len(a.failures[ip]) >= 8 || len(a.failures["global"]) >= 64 || len(a.failures) > 4096 {
		return "", errors.New("too many attempts")
	}
	c, err := a.load()
	if err != nil {
		return "", err
	}
	if !verifyPassword(c, password) {
		a.failures[ip] = append(a.failures[ip], now)
		a.failures["global"] = append(a.failures["global"], now)
		return "", errors.New("invalid password")
	}
	values, err := a.sessionDoc()
	if err != nil {
		return "", err
	}
	a.prune(values)
	if len(values) >= 8192 {
		return "", errors.New("session capacity reached")
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(map[string]any{"n": hex.EncodeToString(nonce), "exp": now + sessionTTL, "epoch": c.Epoch})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	token := payload + "." + signature(c.Secret, payload)
	values[sessionID(token)] = session{Exp: now + sessionTTL, Epoch: c.Epoch, Created: now, Last: now}
	if err := writeJSON(a.sessions, values); err != nil {
		return "", err
	}
	delete(a.failures, ip)
	return token, nil
}
func (a *authStore) verify(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(token) > 2048 {
		return false
	}
	payload, sig, found := strings.Cut(token, ".")
	if !found {
		return false
	}
	c, err := a.load()
	if err != nil || !hmac.Equal([]byte(sig), []byte(signature(c.Secret, payload))) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	var claims struct{ Exp, Epoch int64 }
	if privatefile.Decode(raw, &claims) != nil {
		return false
	}
	if claims.Epoch == 0 {
		claims.Epoch = 1
	}
	now := a.now().Unix()
	if claims.Exp <= now || claims.Epoch != c.Epoch {
		return false
	}
	values, err := a.sessionDoc()
	if err != nil {
		return false
	}
	value, ok := values[sessionID(token)]
	last := value.Last
	if last == 0 {
		last = value.Created
	}
	if !ok || value.Exp <= now || value.Epoch != c.Epoch || last <= 0 || now-last > idleTTL {
		return false
	}
	// Batch touches avoid an fsync for every GUI poll while keeping the inactivity
	// bound conservative across restarts (at most one minute earlier expiry).
	if now-value.Last >= 60 {
		value.Last = now
		values[sessionID(token)] = value
		a.prune(values)
		if writeJSON(a.sessions, values) != nil {
			return false
		}
	}
	return true
}
func (a *authStore) logout(token string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	values, err := a.sessionDoc()
	if err != nil {
		return err
	}
	delete(values, sessionID(token))
	a.prune(values)
	return writeJSON(a.sessions, values)
}
func (a *authStore) changePassword(old, next string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(next) < 16 || len(next) > 4096 {
		return errors.New("new password must contain at least 16 characters")
	}
	c, err := a.load()
	if err != nil || !verifyPassword(c, old) {
		return errors.New("invalid current password")
	}
	var doc map[string]json.RawMessage
	if readJSON(a.config, 2*1024*1024, true, &doc) != nil {
		return errors.New("invalid GUI data")
	}
	var admin map[string]json.RawMessage
	if json.Unmarshal(doc["admin"], &admin) != nil || admin == nil {
		return errors.New("invalid admin data")
	}
	hash, err := passwordHash(next)
	if err != nil {
		return err
	}
	admin["scheme"], _ = json.Marshal("argon2id")
	admin["hash"], _ = json.Marshal(hash)
	delete(admin, "salt")
	doc["admin"], _ = json.Marshal(admin)
	doc["session_epoch"], _ = json.Marshal(c.Epoch + 1)
	doc["updated_at"] = json.RawMessage(strconv.FormatInt(a.now().Unix(), 10))
	// Epoch publication revokes every old session even if clearing the session
	// document subsequently fails; no password/pricing root data is reset.
	if err := writeJSON(a.config, doc); err != nil {
		return err
	}
	return writeJSON(a.sessions, map[string]session{})
}
