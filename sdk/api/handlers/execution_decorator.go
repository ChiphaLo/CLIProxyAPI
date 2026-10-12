package handlers

import "context"

type executionDecoratorKey struct{}

// WithExecutionContextDecorator lets trusted admission middleware transfer a
// small immutable metadata snapshot into the handler's execution context.
// The decorator must derive from its supplied parent and preserve cancellation.
// It is not settable through client JSON or headers.
func WithExecutionContextDecorator(ctx context.Context, decorate func(context.Context) context.Context) context.Context {
	if decorate == nil {
		return ctx
	}
	return context.WithValue(ctx, executionDecoratorKey{}, decorate)
}

func decorateExecutionContext(parent, request context.Context) context.Context {
	if request == nil {
		return parent
	}
	decorate, _ := request.Value(executionDecoratorKey{}).(func(context.Context) context.Context)
	if decorate != nil {
		if decorated := decorate(parent); decorated != nil {
			return decorated
		}
	}
	return parent
}
