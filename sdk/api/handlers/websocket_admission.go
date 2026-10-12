package handlers

import (
	"context"
	"errors"
)

type websocketAdmissionKey struct{}

// WebsocketAdmission is installed only by trusted server middleware. Controls
// are checked in the reader; turns are admitted serially before model routing.
// Neither hook can be set through request headers or JSON.
type WebsocketAdmission struct {
	MaxBytes   int64
	CheckFrame func([]byte) error
	AdmitTurn  func(context.Context, []byte) (context.Context, []byte, string, error)
}

func WithWebsocketAdmission(ctx context.Context, admission *WebsocketAdmission) context.Context {
	return context.WithValue(ctx, websocketAdmissionKey{}, admission)
}

func WebsocketAdmissionFromContext(ctx context.Context) *WebsocketAdmission {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(websocketAdmissionKey{}).(*WebsocketAdmission)
	return v
}

// AdmissionError exposes a safe public message, never raw credentials/payloads.
type AdmissionError struct {
	Status  int
	Message string
}

func (e *AdmissionError) Error() string { return e.Message }
func AdmissionErrorStatus(err error) int {
	var e *AdmissionError
	if errors.As(err, &e) {
		return e.Status
	}
	return 400
}
