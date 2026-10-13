//go:build linux

package adminhelper

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"testing"
)

func newKeyRequest(key string) (*http.Request, error) {
	r, err := http.NewRequest(http.MethodPost, "http://local/v1/responses", nil)
	if err == nil {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	return r, err
}

func readResponse(t *testing.T, conn net.Conn) Result {
	t.Helper()
	raw, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var result Result
	if len(raw) > MaxResponse || json.Unmarshal(raw, &result) != nil {
		t.Fatal("invalid bounded response")
	}
	return result
}
