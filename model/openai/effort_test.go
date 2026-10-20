package openai

import (
	"context"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestAutoSendsASetEffortToALocalServer checks that the effort of the
// configuration or the profile reaches a server that is not OpenAI as
// reasoning_effort, with the token limit still in max_tokens, and that the
// default effort and the option off send nothing (scenario1 K9).
func TestAutoSendsASetEffortToALocalServer(t *testing.T) {
	t.Setenv(ReasoningEnv, "")
	t.Setenv(APIKeySecret, "")
	t.Setenv(BaseURLSecret, "")
	ctx := context.Background()
	cases := []struct {
		name, effort, reasoning string
		req                     fw.Request
		want                    any // reasoning_effort in the body, nil for none
	}{
		{"configuration low", "low", "", fw.Request{}, "low"},
		{"profile medium", "high", "", fw.Request{Effort: "medium", EffortSet: true}, "medium"},
		{"profile max maps to high", "low", "", fw.Request{Effort: "max", EffortSet: true}, "high"},
		{"default high", "high", "", fw.Request{}, nil},
		{"provider default", "low", "", fw.Request{EffortSet: true}, nil},
		{"option off", "low", "off", fw.Request{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFake(t, reply{body: streamBody})
			cfg := fw.DefaultConfig()
			cfg.Effort = tc.effort
			pc := fw.ProviderConfig{BaseURL: srv.URL}
			if tc.reasoning != "" {
				pc.Options = map[string]string{"reasoning": tc.reasoning}
			}
			cfg.Providers = map[string]fw.ProviderConfig{Provider: pc}
			m, err := New(ctx, "qwen3.8-flash", cfg, fw.EnvSecrets{})
			if err != nil {
				t.Fatal(err)
			}
			req := tc.req
			req.Messages = []fw.Message{fw.UserText("hi")}
			req.MaxTokens = 1000
			if _, err := m.Stream(ctx, &req, nil); err != nil {
				t.Fatal(err)
			}
			body := f.bodies[0]
			if got := body["reasoning_effort"]; got != tc.want {
				t.Fatalf("reasoning_effort = %v, want %v", got, tc.want)
			}
			if body["max_tokens"] != float64(1000) || body["max_completion_tokens"] != nil {
				t.Fatalf("token limit = %v / %v", body["max_tokens"], body["max_completion_tokens"])
			}
		})
	}
}

// TestAutoSendsNoEffortToOpenAIForAModelThatDoesNotReason checks that the
// OpenAI and Azure hosts keep the rule of the model name.
func TestAutoSendsNoEffortToOpenAIForAModelThatDoesNotReason(t *testing.T) {
	for _, base := range []string{"https://api.openai.com/v1", "https://team.openai.azure.com/openai/v1", "https://x.cognitiveservices.azure.com/openai/v1"} {
		m := testModelWith(t, settings{base: base, model: "gpt-4.1", effort: "low", effortAuto: true})
		r := fw.Request{Effort: "low"}
		if o := m.bodyOptions(&r); o.Effort || o.Reasoning {
			t.Fatalf("%s: body options %+v", base, o)
		}
	}
	m := testModelWith(t, settings{base: "http://127.0.0.1:8080/v1", model: "qwen", effortAuto: true})
	r := fw.Request{Effort: "low"}
	if o := m.bodyOptions(&r); !o.Effort || o.Reasoning {
		t.Fatalf("local server: body options %+v", o)
	}
	if fw.DefaultConfig().Effort != defaultEffort {
		t.Fatalf("the default effort of fw is %q, and the adapter assumes %q", fw.DefaultConfig().Effort, defaultEffort)
	}
}
