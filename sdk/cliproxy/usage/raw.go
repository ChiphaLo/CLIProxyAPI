package usage

import "context"

type rawUsageCaptureKey struct{}

// WithRawUsageCapture opts a request into retaining unnormalized upstream usage.
// This does not replace Detail or change upstream accounting for other sinks.
func WithRawUsageCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, rawUsageCaptureKey{}, true)
}

func RawUsageCaptureEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(rawUsageCaptureKey{}).(bool)
	return enabled
}
