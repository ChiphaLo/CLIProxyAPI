package auth

import (
	"context"
	"errors"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type executionValidationKey struct{}

// A local policy rejection is not an upstream authentication failure. Keep it
// terminal and outside availability/quota observations and credential retries.
type executionValidationError struct{ cause error }

func (e *executionValidationError) Error() string {
	return "execution credential policy rejected the request"
}
func (e *executionValidationError) Unwrap() error         { return e.cause }
func (e *executionValidationError) StatusCode() int       { return 403 }
func (e *executionValidationError) IsRequestScoped() bool { return true }

func isExecutionValidationError(err error) bool {
	var rejected *executionValidationError
	return errors.As(err, &rejected)
}

// WithExecutionAuthValidator installs an execution-local, read-only policy on
// the credential actually selected by the manager. It is opt-in and cannot be
// configured through request JSON or headers. Retries and credits use the same
// policy; no protocol payload fields are changed by this hook.
func WithExecutionAuthValidator(ctx context.Context, validate func(*Auth) error) context.Context {
	if validate == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, executionValidationKey{}, validate)
}

func validateExecutionAuth(ctx context.Context, auth *Auth) error {
	if ctx != nil {
		if validate, ok := ctx.Value(executionValidationKey{}).(func(*Auth) error); ok && validate != nil {
			if err := validate(auth); err != nil {
				return &executionValidationError{cause: err}
			}
		}
	}
	return nil
}

func executeValidated(ctx context.Context, executor ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := validateExecutionAuth(ctx, auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return executor.Execute(ctx, auth, req, opts)
}

func countTokensValidated(ctx context.Context, executor ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := validateExecutionAuth(ctx, auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return executor.CountTokens(ctx, auth, req, opts)
}

func executeStreamValidated(ctx context.Context, executor ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := validateExecutionAuth(ctx, auth); err != nil {
		return nil, err
	}
	return executor.ExecuteStream(ctx, auth, req, opts)
}
