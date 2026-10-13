//go:build !linux

package privatecontrol

import (
	"context"
	"errors"
	"net"
	"net/http"
)

func Dial(context.Context, string, uint32) (net.Conn, error) {
	return nil, errors.New("authenticated control requires Linux")
}

func Listen(string, uint32) (net.Listener, error) {
	return nil, errors.New("authenticated control requires Linux")
}
func Client(string, uint32) *http.Client { return &http.Client{Transport: rejectedTransport{}} }

type rejectedTransport struct{}

func (rejectedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("authenticated control requires Linux")
}
