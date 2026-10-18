package fw

import (
	"context"
	"errors"
	"testing"
)

// A configuration that names no model boots, serves, and refuses a run
// until the profile of the run names a model.
func TestNoModelConfigured(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	cfg := DefaultConfig()
	if cfg.Model != "" {
		t.Fatalf("DefaultConfig names the model %q; the kernel must have no default model", cfg.Model)
	}
	cfg.Agents = map[string]AgentProfile{
		"default": {},
		"pinned":  {Model: "anthropic/claude-opus-5-5"},
	}
	a, err := New(WithoutDefaults(), WithAuthn(NoAuthn{}), WithConfig(cfg), WithLedger(NewMemLedger()), WithBus(NewMemBus()), WithLogger(quietLogger()))
	if err != nil {
		t.Fatalf("New without a model: %v", err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	if a.Model() != nil {
		t.Fatal("Model() must be nil when the configuration names none")
	}
	ctx := testCtx(t)
	if _, err := a.ModelFor(ctx, ""); !errors.Is(err, ErrNoModel) {
		t.Fatalf("ModelFor(\"\") = %v; want ErrNoModel", err)
	}
	_, err = a.Start(ctx, "hello")
	if !errors.Is(err, ErrNoModel) || !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Start without a model = %v; want ErrInvalidRequest wrapping ErrNoModel", err)
	}
	// A profile that names a model passes the request check. The run can
	// still fail later, because this package registers no provider.
	if _, err := a.Start(ctx, "hello", AsAgent("pinned")); errors.Is(err, ErrNoModel) {
		t.Fatalf("Start with a profile model = %v; the request must not be refused for a missing model", err)
	}
}
