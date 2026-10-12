package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type validationExecutor struct {
	calls, prepares, results int
	req                      exec.Request
	opts                     exec.Options
}

func (*validationExecutor) Identifier() string { return "codex" }
func (e *validationExecutor) Execute(_ context.Context, _ *Auth, req exec.Request, opts exec.Options) (exec.Response, error) {
	e.calls++
	e.req, e.opts = req, opts
	return exec.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (e *validationExecutor) CountTokens(ctx context.Context, auth *Auth, req exec.Request, opts exec.Options) (exec.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}
func (e *validationExecutor) ExecuteStream(ctx context.Context, auth *Auth, req exec.Request, opts exec.Options) (*exec.StreamResult, error) {
	_, _ = e.Execute(ctx, auth, req, opts)
	chunks := make(chan exec.StreamChunk, 1)
	chunks <- exec.StreamChunk{Payload: []byte(`data: {"ok":true}`)}
	close(chunks)
	return &exec.StreamResult{Chunks: chunks}, nil
}
func (*validationExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }
func (*validationExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected HTTP")
}
func (*validationExecutor) ShouldPrepareRequestAuth(*Auth) bool { return true }
func (e *validationExecutor) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	e.prepares++
	return auth, nil
}

func TestExecutionValidatorIsReadOnlyAndOptIn(t *testing.T) {
	req := exec.Request{Model: "test-model", Payload: []byte(`{"model":"test-model","reasoning":{"effort":"high"}}`)}
	opts := exec.Options{Headers: http.Header{"X-Test": {"same"}}, Metadata: map[string]any{"keep": "same"}, OriginalRequest: []byte(`{"original":true}`)}
	auth := &Auth{ID: "validation-read-only", Provider: "codex"}
	for _, enabled := range []bool{false, true} {
		ctx := context.Background()
		checked := 0
		if enabled {
			ctx = WithExecutionAuthValidator(ctx, func(got *Auth) error {
				checked++
				if got != auth {
					t.Fatal("credential changed")
				}
				return nil
			})
		}
		e := &validationExecutor{}
		if _, err := executeValidated(ctx, e, auth, req, opts); err != nil {
			t.Fatal(err)
		}
		if _, err := countTokensValidated(ctx, e, auth, req, opts); err != nil {
			t.Fatal(err)
		}
		if _, err := executeStreamValidated(ctx, e, auth, req, opts); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(e.req, req) || !reflect.DeepEqual(e.opts, opts) || e.calls != 3 || (enabled && checked != 3) || (!enabled && checked != 0) {
			t.Fatal("validation changed payload/options or default execution")
		}
	}
}

func TestPolicyRejectionDoesNotPrepareCallRetryOrPoisonAvailability(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(mode, func(t *testing.T) {
			e := &validationExecutor{}
			m := NewManager(nil, nil, nil)
			m.RegisterExecutor(e)
			m.SetRetryConfig(2, 0, 2)
			for _, id := range []string{"validation-primary", "validation-secondary"} {
				if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive}); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "validation-model"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			m.SetResultPolicy(ResultPolicyFunc(func(_ context.Context, result Result) Result { e.results++; return result }))
			checks := 0
			ctx := WithExecutionAuthValidator(context.Background(), func(*Auth) error { checks++; return errors.New("private member mismatch") })
			req := exec.Request{Model: "validation-model", Payload: []byte(`{"model":"validation-model"}`)}
			var err error
			switch mode {
			case "execute":
				_, err = m.Execute(ctx, []string{"codex"}, req, exec.Options{})
			case "count":
				_, err = m.ExecuteCount(ctx, []string{"codex"}, req, exec.Options{})
			case "stream":
				_, err = m.ExecuteStream(ctx, []string{"codex"}, req, exec.Options{})
			}
			if !isExecutionValidationError(err) || checks != 1 || e.calls != 0 || e.prepares != 0 || e.results != 0 || hasUpstreamExecutionAttempt(err) {
				t.Fatalf("policy rejection became an upstream attempt: checks=%d calls=%d prepares=%d results=%d err=%v", checks, e.calls, e.prepares, e.results, err)
			}
			for _, id := range []string{"validation-primary", "validation-secondary"} {
				auth, _ := m.GetByID(id)
				if auth.Unavailable || len(auth.ModelStates) != 0 {
					t.Fatal("local rejection poisoned credential availability")
				}
			}
		})
	}
}
