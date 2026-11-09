package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"
)

const BridgeHeader = "X-Cpa-Request-Identity"

// SignBridge binds an internal identity to the exact final request and the
// existing authenticated transport key. No credential is carried in the claim.
func SignBridge(key string, payload []byte, derived string, now time.Time) string {
	if key == "" || !strings.HasPrefix(derived, identityPrefix) {
		return ""
	}
	stamp := strconv.FormatInt(now.Unix(), 10)
	claim := "v1." + stamp + "." + derived
	return claim + "." + bridgeMAC(key, payload, claim)
}

func bridgeMAC(key string, payload []byte, claim string) string {
	digest := blake2b.Sum256(payload)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("cpa:internal-responses-identity:v1\x00"))
	mac.Write([]byte(claim))
	mac.Write(digest[:])
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyBridge consumes the private header even when invalid. Authentication,
// freshness and full-body binding are required before returning an identity.
func VerifyBridge(headers http.Header, key string, payload []byte, now time.Time) string {
	values := headers.Values(BridgeHeader)
	headers.Del(BridgeHeader)
	if len(values) != 1 || len(values[0]) > 256 || key == "" {
		return ""
	}
	parts := strings.Split(values[0], ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return ""
	}
	stamp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || now.Unix()-stamp > 120 || stamp-now.Unix() > 10 {
		return ""
	}
	id := parts[2]
	if !strings.HasPrefix(id, identityPrefix) || len(id) != len(identityPrefix)+64 {
		return ""
	}
	if _, err = hex.DecodeString(strings.TrimPrefix(id, identityPrefix)); err != nil {
		return ""
	}
	sig, err := hex.DecodeString(parts[3])
	if err != nil {
		return ""
	}
	want, _ := hex.DecodeString(bridgeMAC(key, payload, strings.Join(parts[:3], ".")))
	if !hmac.Equal(sig, want) {
		return ""
	}
	return id
}
