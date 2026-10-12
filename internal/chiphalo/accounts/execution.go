package accounts

import (
	"context"
	"fmt"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type bindingError struct{}

func (bindingError) Error() string   { return "CPA OAuth account binding validation failed" }
func (bindingError) StatusCode() int { return 403 }

// Decorate validates the exact selected credential, not a client-supplied
// prefix or a shared workspace ID. Unknown OAuth models cannot use the
// upstream's unprefixed pool as an accidental fallback route.
func (r *Registry) Decorate(ctx context.Context, expectedAuthID string) context.Context {
	return coreauth.WithExecutionAuthValidator(ctx, func(auth *coreauth.Auth) error {
		if auth == nil {
			return bindingError{}
		}
		if expectedAuthID != "" && auth.ID != expectedAuthID {
			return bindingError{}
		}
		if auth.Provider != "codex" {
			if expectedAuthID != "" {
				return bindingError{}
			}
			return nil
		}
		if expectedAuthID == "" || r.ValidateAuth(auth) != nil {
			return bindingError{}
		}
		return nil
	})
}

func (r *Registry) BindingForAuth(id string) (Binding, error) {
	if binding, known := r.byAuth[id]; known {
		return binding, nil
	}
	return Binding{}, fmt.Errorf("OAuth credential is not registered")
}
