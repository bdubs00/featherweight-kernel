package fw

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactorValuesFromEnvironment(t *testing.T) {
	r := newRedactor([]string{
		"OPENAI_API_KEY=sk-local-1234567890",
		"FW_API_TOKEN=tok-abcdefgh\n",
		"DB_PASSWORD=hunter2hunter2",
		"FW_AUTH_TOKEN_ENV=FW_API_TOKEN",          // names where a secret is
		"GOOGLE_CREDENTIALS_FILE=/run/creds.json", // names where a secret is
		"SHORT_TOKEN=abc",                         // too short to replace
		"HOME=/home/someone",
	})
	in := "key sk-local-1234567890, token tok-abcdefgh, pw hunter2hunter2, env FW_API_TOKEN, file /run/creds.json, short abc, home /home/someone"
	want := "key [redacted], token [redacted], pw [redacted], env FW_API_TOKEN, file /run/creds.json, short abc, home /home/someone"
	if got := r.text(in); got != want {
		t.Fatalf("text:\n got %q\nwant %q", got, want)
	}
}

func TestRedactorShapes(t *testing.T) {
	r := newRedactor(nil)
	cases := []struct{ in, want string }{
		{"token ghp_abcdefghijklmnopqrstuvwxyz0123456789 end", "token [redacted] end"},
		{"key sk-ant-api03-abcdefghijklmnopqrstuvwxyz", "key [redacted]"},
		{"aws AKIAABCDEFGHIJKLMNOP", "aws [redacted]"},
		{"Authorization: Bearer abcdefghijklmnop.qrstuv", "Authorization: Bearer [redacted]"},
		{"url = https://x-access-token:ghs_secret@github.com/o/r.git", "url = https://[redacted]@github.com/o/r.git"},
		{"url = https://TOKEN123@github.com/o/r.git", "url = https://[redacted]@github.com/o/r.git"},
		{"nats://fw:pass@nats:4222", "nats://[redacted]@nats:4222"},
		{"ssh://git@github.com/o/r.git stays", "ssh://git@github.com/o/r.git stays"},
		{"https://example.com/?to=a@b.com stays", "https://example.com/?to=a@b.com stays"},
		{"a\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn\nNzaC1\n-----END OPENSSH PRIVATE KEY-----\nz", "a\n[redacted]\nz"},
		{"jwt eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhbGljZSJ9.c2lnbmF0dXJlLXZhbHVl", "jwt [redacted]"},
		{"plain text with sk-short stays", "plain text with sk-short stays"},
	}
	for _, c := range cases {
		if got := r.text(c.in); got != c.want {
			t.Errorf("text(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestRedactorData(t *testing.T) {
	r := newRedactor([]string{"GITHUB_TOKEN=ghx-0123456789abcdef"})
	clean := json.RawMessage(`{"callId":"c1","content":[{"type":"text","text":"nothing here"}]}`)
	if got := r.data(clean); string(got) != string(clean) {
		t.Fatalf("clean data changed: %s", got)
	}
	// The base64 text of these bytes has the shape of an AWS key.
	img, err := base64.StdEncoding.DecodeString("AKIAABCDEFGHIJKLMNOP+abc")
	if err != nil {
		t.Fatal(err)
	}
	resp := ToolRespondedData{CallID: "c1", Content: []Block{
		Text("line one\nghx-0123456789abcdef and https://u:p@host/x"),
		{Type: BlockImage, MediaType: "image/png", Data: img},
		{Type: BlockOpaque, Provider: "anthropic", Raw: json.RawMessage(`{"thinking":"ghx-0123456789abcdef","signature":"s"}`)},
	}}
	raw, _ := json.Marshal(resp)
	if !strings.Contains(string(raw), "AKIAABCDEFGHIJKLMNOP+abc") {
		t.Fatalf("the test image does not hold the shape: %s", raw)
	}
	out := r.data(raw)
	if !json.Valid(out) {
		t.Fatalf("invalid JSON: %s", out)
	}
	var got ToolRespondedData
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if want := "line one\n[redacted] and https://[redacted]@host/x"; got.Content[0].Text != want {
		t.Errorf("text = %q, want %q", got.Content[0].Text, want)
	}
	if string(got.Content[1].Data) != string(img) {
		t.Errorf("image bytes changed: %q", got.Content[1].Data)
	}
	if !strings.Contains(string(got.Content[2].Raw), "ghx-0123456789abcdef") {
		t.Errorf("a signed opaque block changed: %s", got.Content[2].Raw)
	}
}

func TestLedgerHoldsNoSecret(t *testing.T) {
	ctx := testCtx(t)
	const key = "zz-test-secret-0123456789"
	t.Setenv("FW_TEST_TOKEN", key)
	leak := NewTool(ToolSpec{Name: "leak", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return TextResult("the key is " + key + " and the vault says " + "vault-value-987654"), nil
	})
	vault := SecretsFunc(func(_ context.Context, name string) (string, error) {
		return "vault-value-987654", nil
	})
	s := newScript(callTools(use("c1", "leak", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(leak), WithSecrets(vault))
	if _, err := a.Secrets().Get(ctx, "VAULT_ITEM"); err != nil {
		t.Fatal(err)
	}
	r, err := a.Run(ctx, "show me "+key)
	mustState(t, r, err, StateCompleted)
	for _, e := range ledgerEvents(t, a.Ledger(), r.ID) {
		if strings.Contains(string(e.Data), key) || strings.Contains(string(e.Data), "vault-value-987654") {
			t.Fatalf("%s holds a secret: %s", e.Type, e.Data)
		}
	}
	reqs := s.requests()
	if got := TextOf(reqs[0].Messages[0].Content); got != "show me [redacted]" {
		t.Errorf("input reached the model as %q", got)
	}
	if got := TextOf(lastToolResults(t, reqs[1])[0].Content); got != "the key is [redacted] and the vault says [redacted]" {
		t.Errorf("tool result reached the model as %q", got)
	}
	if got := a.Redact("log " + key); got != "log [redacted]" {
		t.Errorf("Redact = %q", got)
	}
}
