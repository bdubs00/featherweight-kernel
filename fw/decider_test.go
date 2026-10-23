package fw

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// tokenDecider is the decider of the driver of the test: it keeps the
// token that its constructor read.
type tokenDecider struct{ token string }

func (d tokenDecider) Decide(context.Context, string, map[string]Question) (DecideResult, error) {
	return DecideResult{Model: d.token}, nil
}

// The driver of an entry of deciders builds its client from the entry and
// reads the secret of the token through the secrets of the agent, so the
// redactor learns the token. An entry without a driver takes systemone, an
// unknown name or a driver that the binary lacks is an error that names
// it, and WithDecider wins over the driver.
func TestDeciderNamedBuildsFromTheDriver(t *testing.T) {
	RegisterDecider("test-token", func(ctx context.Context, name string, dc DeciderConfig, secrets Secrets) (Decider, error) {
		v, err := secrets.Get(ctx, dc.APIKeyEnv)
		if err != nil {
			return nil, err
		}
		return tokenDecider{token: name + ":" + v}, nil
	})
	if !slices.Contains(RegisteredDeciders(), "test-token") {
		t.Fatalf("registered = %v", RegisteredDeciders())
	}
	cfg := DefaultConfig()
	cfg.Deciders = map[string]DeciderConfig{
		"clef":   {Driver: "Test-Token", BaseURL: "http://127.0.0.1:1", APIKeyEnv: "CLEF_API_KEY", Model: "clef"},
		"jev":    {Driver: "nowhere", BaseURL: "http://127.0.0.1:1", Model: "jev"},
		"plain":  {BaseURL: "http://127.0.0.1:1", Model: "clef"},
		"faked":  {BaseURL: "http://127.0.0.1:1", Model: "clef"},
		"unused": {Driver: "test-token", BaseURL: "http://127.0.0.1:1", Model: "clef"},
	}
	secrets := SecretsFunc(func(_ context.Context, name string) (string, error) {
		if name == "CLEF_API_KEY" {
			return "clef-bearer-0123456789", nil
		}
		return "", ErrSecretNotFound
	})
	a, err := New(WithoutDefaults(), WithAuthn(NoAuthn{}), WithConfig(cfg), WithLedger(NewMemLedger()), WithBus(NewMemBus()),
		WithSecrets(secrets), WithLogger(quietLogger()), WithDecider("faked", tokenDecider{token: "fake"}), WithDecider("given", tokenDecider{token: "given"}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	if got := strings.Join(a.Deciders(), ","); got != "clef,faked,given,jev,plain,unused" {
		t.Errorf("deciders = %s", got)
	}
	d, err := a.DeciderNamed(context.Background(), "clef")
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := d.Decide(context.Background(), "x", nil); res.Model != "clef:clef-bearer-0123456789" {
		t.Errorf("the driver built %+v", d)
	}
	// The redactor knows the token now, so no event holds it.
	if got := a.Redact("token clef-bearer-0123456789"); strings.Contains(got, "clef-bearer") {
		t.Errorf("redact = %q", got)
	}
	for name, want := range map[string]string{"faked": "fake", "given": "given"} {
		if d, err := a.DeciderNamed(context.Background(), name); err != nil || d.(tokenDecider).token != want {
			t.Errorf("%s = %v, %v; want the decider of WithDecider", name, d, err)
		}
	}
	if _, err := a.DeciderNamed(context.Background(), "jev"); err == nil || !strings.Contains(err.Error(), `no decider driver "nowhere" is linked into this binary`) {
		t.Errorf("unknown driver: %v", err)
	}
	if _, err := a.DeciderNamed(context.Background(), "plain"); err == nil || !strings.Contains(err.Error(), `no decider driver "systemone"`) {
		t.Errorf("an entry without a driver takes systemone, which package fw does not link: %v", err)
	}
	if _, err := a.DeciderNamed(context.Background(), "nobody"); err == nil || !strings.Contains(err.Error(), `no decider "nobody"`) {
		t.Errorf("unknown decider: %v", err)
	}
}
