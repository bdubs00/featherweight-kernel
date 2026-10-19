package fw

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"math/rand"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// The quick check of the raw JSON of an event never misses a secret that
// the pass over its strings replaces, also behind a JSON escape, so the
// redaction of an event and the redaction of each of its strings agree.
func TestRedactorDataSeesThroughEscapes(t *testing.T) {
	r := newRedactor([]string{"VAULT_TOKEN=vault\"quoted\\value"})
	for _, s := range []string{
		"Authorization: Bearer\tabcdefghijklmnopqrstuvwxyz0123.",
		"Authorization: Bearer \t  abcdefghijklmnopqrstuvwxyz0123",
		"key\tAKIAIOSFODNN7EXAMPLE\tend",
		"https://bot\t@host.example/x and https://bot@host.example/y",
		"the value vault\"quoted\\value of the variable",
		"a <b> & c with ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"nothing \t secret \n here",
	} {
		raw, err := json.Marshal(map[string]string{"text": s})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := json.Unmarshal(r.data(raw), &got); err != nil {
			t.Fatal(err)
		}
		if want := r.text(s); got["text"] != want {
			t.Errorf("data(%q) = %q, want %q", s, got["text"], want)
		}
	}
	// A \u escape can spell a key, so the quick check reads the escape.
	raw := json.RawMessage(`{"text":"key AKIAIOSFODNN7EXAMPLE and 😀 ghp_abcdefghijklmnopqrstuvwxyz0123456789"}`)
	var got map[string]string
	if err := json.Unmarshal(r.data(raw), &got); err != nil {
		t.Fatal(err)
	}
	if want := "key [redacted] and \U0001F600 [redacted]"; got["text"] != want {
		t.Errorf("text = %q, want %q", got["text"], want)
	}
	if !bytes.Equal(unescapeJSON([]byte(`a\"b\\c\/d\teé😀\q`)), []byte("a\"b\\c/d\teé\U0001F600q")) {
		t.Errorf("unescapeJSON = %q", unescapeJSON([]byte(`a\"b\\c\/d\teé😀\q`)))
	}
}

// The bearer shape replaces a token after the word Bearer and leaves a key,
// a variable, or a word after it alone: in YAML and Prometheus, nginx,
// curl, Go code, and prose. Data in which nothing changes keeps its bytes.
func TestRedactorKeepsWordsAfterBearer(t *testing.T) {
	r := newRedactor(nil)
	for _, s := range []string{
		"authorization:\n  type: Bearer\n  credentials_file: /var/run/secrets/kubernetes.io/serviceaccount/token\n",
		"components:\n  securitySchemes:\n    api:\n      type: http\n      scheme: bearer\n      bearerFormat: JWT\n      description_of_scheme: tokens\n",
		"location / {\n    proxy_set_header Authorization \"Bearer $access_token\";\n}\n",
		"curl -H \"Authorization: Bearer $TOKEN\" https://api.example.com/v1/items",
		"curl -H 'Authorization: Bearer ${API_TOKEN}' https://api.example.com",
		"req.Header.Set(\"Authorization\", \"Bearer \"+token)\nreturn fmt.Sprintf(\"Bearer %s\", tok)\n",
		"// Bearer\n// authorizationHeaderValue returns the header.\nfunc authorizationHeaderValue() string { return \"\" }\n",
		"Our API accepts bearer authentication/authorization headers.",
		"The handler checks the Bearer authentication_middleware first.",
	} {
		if got := r.text(s); got != s {
			t.Errorf("text(%q)\n got %q", s, got)
		}
		raw := json.RawMessage(`{"z":1,"content":` + string(jsonQuote(s)) + `}`)
		if got := r.data(raw); string(got) != string(raw) {
			t.Errorf("data(%s)\n got %s", raw, got)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"Authorization: Bearer abcdefghijklmnop.qrstuv", "Authorization: Bearer [redacted]"},
		{"Authorization: Bearer\tabcdefghijklmnopqrstuvwxyz0123", "Authorization: Bearer\t[redacted]"},
		{"Bearer    AbCdEfGhIjKlMnOpQrStUv", "Bearer    [redacted]"},
		{"curl -H 'Authorization: Bearer 0123456789abcdefghij' https://x", "curl -H 'Authorization: Bearer [redacted]' https://x"},
		{"token=Bearer 9f8e7d6c5b4a39281706", "token=Bearer [redacted]"},
	} {
		if got := r.text(c.in); got != c.want {
			t.Errorf("text(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func jsonQuote(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

// A tool that writes a Prometheus block with "type: Bearer" gets the block
// as the model wrote it, and a tool that reads it gives the model the
// block as the tool returned it.
func TestToolInputWithBearerAtLineEndStays(t *testing.T) {
	ctx := testCtx(t)
	conf := "authorization:\n  type: Bearer\n  credentials_file: /var/run/secrets/kubernetes.io/serviceaccount/token\n"
	in, _ := json.Marshal(map[string]string{"path": "prometheus.yml", "content": conf})
	s := newScript(callTools(use("c1", "write", string(in)), use("c2", "read", `{}`)), endTurn("done"))
	var written atomic.Value
	write := NewTool(ToolSpec{Name: "write", ReadOnly: true}, func(_ context.Context, raw json.RawMessage) (ToolResult, error) {
		var v struct{ Content string }
		_ = json.Unmarshal(raw, &v)
		written.Store(v.Content)
		return TextResult("ok"), nil
	})
	read := NewTool(ToolSpec{Name: "read", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return TextResult(conf), nil
	})
	a := newTestAgent(t, s.Model(), WithTools(write, read))
	r, err := a.Run(ctx, "write the prometheus config")
	mustState(t, r, err, StateCompleted)
	if got, _ := written.Load().(string); got != conf {
		t.Errorf("the tool wrote %q, want %q", got, conf)
	}
	if got := TextOf(lastToolResults(t, s.requests()[1])[1].Content); got != conf {
		t.Errorf("the model read %q, want %q", got, conf)
	}
}

// Whenever the pass over the strings of an event changes one, the quick
// check of its raw JSON is true, also with escapes, control characters,
// and invalid UTF-8 around the shapes.
func TestRedactorQuickCheckIsSuperset(t *testing.T) {
	r := newRedactor([]string{"GITHUB_TOKEN=s3cr3t<v@lue>&\"x\\y"})
	pieces := []string{"Bearer", "bearer", "Authorization:", " ", "\t", "\n", "\r", "\b", "\x00", "\x1f", " ",
		"<", ">", "&", "\"", "\\", "/", "@", ":", "https://", "user", "AKIA", "ABCDEFGHIJKLMNOP", "0123456789abcdef",
		"ghp_", "abcdefghijklmnopqrstuvwxyz0123", "credentials_file", "sk-ant-", "eyJ", ".", "😀", "\xff",
		"s3cr3t<v@lue>&\"x\\y", "-----BEGIN RSA PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----", "xoxb-", "=", "_", "-"}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 12000; i++ {
		var b strings.Builder
		for range 1 + rng.Intn(12) {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		raw, _ := json.Marshal(map[string]any{"a": b.String()})
		var back map[string]string
		_ = json.Unmarshal(raw, &back)
		if s := back["a"]; r.text(s) != s && !r.holds(raw) {
			t.Fatalf("the quick check misses %q", s)
		}
	}
}

// The quick check reads an event with many escapes once: it takes about
// the time of one scan, not two.
func TestRedactorHoldsReadsOnce(t *testing.T) {
	r := newRedactor(nil)
	big := strings.Repeat("line of code with a \"quote\" and a tab\there\n", 3000)
	raw, _ := json.Marshal(ToolRespondedData{CallID: "c1", Content: []Block{Text(big)}})
	view := unescapeJSON(raw)
	best := func(f func()) time.Duration {
		var d time.Duration
		for i := range 3 {
			start := time.Now()
			f()
			if e := time.Since(start); i == 0 || e < d {
				d = e
			}
		}
		return d
	}
	scan := best(func() { anyShape.Match(view) })
	check := best(func() { r.holds(raw) })
	if check > scan*3/2 {
		t.Fatalf("the quick check takes %v; one scan takes %v", check, scan)
	}
}
