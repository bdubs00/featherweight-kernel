package fw

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"
)

// Redacted replaces a secret in a ledger event.
const Redacted = "[redacted]"

// minSecretLen is the length of the shortest value that the redactor
// replaces. A shorter value, such as a test token, matches too much text.
const minSecretLen = 8

// secretShapes match text with the shape of a known secret. The match
// becomes the replacement, in which ${1} is the first group. A URL keeps
// its scheme and host and loses its user information: always for http and
// https, which carry a token there, and for another scheme when the user
// information holds a password, so that ssh://git@host keeps its user.
//
// A shape with a group named token replaces a match only when that group
// looks like a token (tokenLike), or when its group named header matched.
// The bearer shape needs it: the word Bearer is common in configuration,
// code, and prose, as in "type: Bearer" of a Prometheus block or "bearer
// authentication", and the word after it is then a key or a word, not a
// token. After "Authorization:", the word is a credential whatever its
// letters. The bearer shape also takes the token on the same line only,
// after spaces or tabs, as an Authorization header holds it, so a key on
// the next line never matches.
var secretShapes = []struct{ pattern, repl string }{
	{`-----BEGIN[A-Z0-9 ]*PRIVATE KEY-----[^"]*?-----END[A-Z0-9 ]*PRIVATE KEY-----`, Redacted},
	{`\bsk-ant-[A-Za-z0-9_-]{20,}`, Redacted},
	{`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{32,}`, Redacted},
	{`\bgh[pousr]_[A-Za-z0-9]{30,}`, Redacted},
	{`\bgithub_pat_[A-Za-z0-9_]{40,}`, Redacted},
	{`\bglpat-[A-Za-z0-9_-]{20,}`, Redacted},
	{`\bxox[abposr]-[A-Za-z0-9-]{10,}`, Redacted},
	{`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`, Redacted},
	{`\bAIza[0-9A-Za-z_-]{35}`, Redacted},
	{`\bhf_[A-Za-z0-9]{30,}`, Redacted},
	{`\bnpm_[A-Za-z0-9]{36}`, Redacted},
	{`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`, Redacted},
	{`(?i)\b((?P<header>authorization:[ \t]*)?bearer[ \t]+)(?P<token>[A-Za-z0-9._~+/-]{16,}=*)`, "${1}" + Redacted},
	{`(?i)\b(https?://)[^/?#\s"'@]+@`, "${1}" + Redacted + "@"},
	{`(?i)\b([a-z][a-z0-9+.-]*://)[^/?#\s"':@]+:[^/?#\s"'@]*@`, "${1}" + Redacted + "@"},
}

// shapes are the compiled secretShapes. anyShape is every shape in one
// pattern without \b, for raw JSON, in which an escape such as "\n" hides
// a word boundary.
var shapes, anyShape = compileShapes()

func compileShapes() ([]*regexp.Regexp, *regexp.Regexp) {
	var out []*regexp.Regexp
	var all []string
	for _, s := range secretShapes {
		out = append(out, regexp.MustCompile(s.pattern))
		all = append(all, "(?:"+strings.ReplaceAll(s.pattern, `\b`, "")+")")
	}
	return out, regexp.MustCompile(strings.Join(all, "|"))
}

// notSecret holds the last words of a variable name that names where a
// secret is, not the secret itself.
var notSecret = map[string]bool{"FILE": true, "PATH": true, "DIR": true, "ENV": true, "NAME": true, "ID": true, "MODE": true, "TYPE": true, "URL": true, "HEADER": true}

// secretName matches the name of a variable that holds a secret, such as
// OPENAI_API_KEY, FW_API_TOKEN, or DB_PASSWORD.
var secretName = regexp.MustCompile(`(?i)(^|_)(API_?KEY|KEY|TOKEN|SECRET|PASSWORD|PASSWD|PASS|PAT|CREDENTIALS?)($|_)`)

// isSecretName reports whether a variable name holds a secret. A name that
// ends in a word such as _FILE, _PATH, or _ENV names where the secret is,
// and is not one.
func isSecretName(name string) bool {
	if !secretName.MatchString(name) {
		return false
	}
	last := name[strings.LastIndexByte(name, '_')+1:]
	return !notSecret[strings.ToUpper(last)]
}

// redactor removes secrets from event data before the kernel appends the
// event. It knows two kinds of secret: the values that fw resolved through
// Secrets or found in a variable with a secret name, and text with the
// shape of a known secret, such as a PEM private key, a GitHub token, or
// the user information of a URL.
type redactor struct {
	mu     sync.RWMutex
	values []string // longest first, so a value that holds another goes first
	// resolved holds the values that fw resolved through Secrets, a subset
	// of values. ToolEnv keeps them from a tool under any name.
	resolved []string
	// own holds the values that a package added with Agent.AddSecret, a
	// subset of values, which Agent.RemoveSecret can take out again.
	own []string
}

// newRedactor returns a redactor that knows the secret values of the
// environment, given as NAME=VALUE pairs.
func newRedactor(environ []string) *redactor {
	r := &redactor{}
	for _, kv := range environ {
		if name, value, ok := strings.Cut(kv, "="); ok && isSecretName(name) {
			r.add(value)
		}
	}
	return r
}

// add records a secret value.
func (r *redactor) add(v string) {
	v = strings.TrimSpace(v)
	if len(v) < minSecretLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.Contains(r.values, v) {
		return
	}
	r.values = append(r.values, v)
	slices.SortFunc(r.values, func(a, b string) int { return len(b) - len(a) })
}

// addResolved records a value that fw resolved through Secrets.
func (r *redactor) addResolved(v string) {
	r.add(v)
	v = strings.TrimSpace(v)
	if len(v) < minSecretLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.resolved, v) {
		r.resolved = append(r.resolved, v)
	}
}

// addOwn records a secret value of a package (Agent.AddSecret).
func (r *redactor) addOwn(v string) {
	r.add(v)
	v = strings.TrimSpace(v)
	if len(v) < minSecretLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.own, v) {
		r.own = append(r.own, v)
	}
}

// removeOwn takes out a value that addOwn recorded, unless fw also
// resolved it through Secrets.
func (r *redactor) removeOwn(v string) {
	v = strings.TrimSpace(v)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.own, v) || slices.Contains(r.resolved, v) {
		return
	}
	r.own = slices.DeleteFunc(r.own, func(x string) bool { return x == v })
	r.values = slices.DeleteFunc(r.values, func(x string) bool { return x == v })
}

// hasSecret reports whether s holds a secret: a value that the redactor
// knows, or text with the shape of a secret.
func (r *redactor) hasSecret(s string) bool { return r != nil && r.text(s) != s }

// hasResolved reports whether s holds a value that fw resolved through
// Secrets.
func (r *redactor) hasResolved(s string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.resolved {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// text returns s with every secret replaced.
func (r *redactor) text(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, Redacted)
	}
	r.mu.RUnlock()
	for i, re := range shapes {
		repl := secretShapes[i].repl
		g := re.SubexpIndex("token")
		if g < 0 {
			s = re.ReplaceAllString(s, repl)
			continue
		}
		h := re.SubexpIndex("header")
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			at := re.FindStringSubmatchIndex(m)
			if at == nil {
				return m
			}
			if (h < 0 || at[2*h] < 0) && !tokenLike(m[at[2*g]:at[2*g+1]]) {
				return m
			}
			return string(re.ExpandString(nil, repl, m, at))
		})
	}
	return s
}

// tokenLike reports whether s has the alphabet of a token and not of a
// word or a key: a digit, or a capital and a small letter. A key such as
// credentials_file or a word such as authentication has neither.
func tokenLike(s string) bool {
	var digit, upper, lower bool
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		}
	}
	return digit || upper && lower
}

// holds reports whether raw JSON can hold a secret. It is the fast path:
// most events hold none, and the kernel appends them unchanged. An escape
// in a JSON string can hide a secret from a look at the raw bytes, such
// as "\t" between the word Bearer and a token, or "\u0041" for the
// letter A of a key. The check therefore reads raw with every escape
// resolved, in which the text of each string appears as the string holds
// it, so it finds every secret that the full pass over the strings finds.
// Raw without an escape is that text already. Either way, the check reads
// the data once.
func (r *redactor) holds(raw []byte) bool {
	view := raw
	if bytes.IndexByte(raw, '\\') >= 0 {
		view = unescapeJSON(raw)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.values {
		if bytes.Contains(view, []byte(v)) || bytes.Contains(view, jsonText(v)) {
			return true
		}
	}
	return anyShape.Match(view)
}

// unescapeJSON returns raw JSON with every escape resolved: the one-letter
// escapes such as \t and \n, and \uXXXX, also as a surrogate pair. The
// result is no longer JSON. The text of each string of raw appears in it
// as the string holds it.
func unescapeJSON(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c != '\\' || i+1 == len(raw) {
			out = append(out, c)
			continue
		}
		i++
		switch e := raw[i]; e {
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, n := hexRune(raw[i+1:])
			if n == 0 {
				out = append(out, '\\', 'u')
				continue
			}
			if utf16.IsSurrogate(r) && len(raw) >= i+1+n+6 && raw[i+1+n] == '\\' && raw[i+2+n] == 'u' {
				if r2, m := hexRune(raw[i+3+n:]); m != 0 {
					if d := utf16.DecodeRune(r, r2); d != utf8.RuneError {
						r, n = d, n+2+m
					}
				}
			}
			out = utf8.AppendRune(out, r)
			i += n
		default:
			out = append(out, e) // \", \\, and \/
		}
	}
	return out
}

// hexRune reads the four hex digits of a \u escape. It returns the rune and
// 4, or 0 when b does not start with four hex digits.
func hexRune(b []byte) (rune, int) {
	if len(b) < 4 {
		return 0, 0
	}
	v, err := strconv.ParseUint(string(b[:4]), 16, 32)
	if err != nil {
		return 0, 0
	}
	return rune(v), 4
}

// jsonText returns v as it appears inside a JSON string.
func jsonText(v string) []byte {
	b, _ := json.Marshal(v)
	return b[1 : len(b)-1]
}

// data returns event data with every secret replaced in its strings. It
// leaves an opaque block alone, because a provider signs it, and it leaves
// the bytes of an image or a document alone. Data in which no string
// changes stays the same bytes, so the input of a tool call and its result
// keep their exact form.
func (r *redactor) data(raw json.RawMessage) json.RawMessage {
	if r == nil || len(raw) == 0 || !r.holds(raw) {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	changed := false
	v = r.walk(v, &changed)
	if !changed {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

// walk replaces the secrets in the strings of v and sets changed when it
// replaced one.
func (r *redactor) walk(v any, changed *bool) any {
	switch t := v.(type) {
	case string:
		out := r.text(t)
		if out != t {
			*changed = true
		}
		return out
	case []any:
		for i := range t {
			t[i] = r.walk(t[i], changed)
		}
	case map[string]any:
		typ := t["type"]
		if typ == BlockOpaque {
			return t
		}
		for k, x := range t {
			if k == "data" && (typ == BlockImage || typ == BlockDocument) {
				continue
			}
			t[k] = r.walk(x, changed)
		}
	}
	return v
}

// recordSecrets passes each value that Secrets resolves to the redactor.
type recordSecrets struct {
	Secrets
	r *redactor
}

// Get implements Secrets.
func (s recordSecrets) Get(ctx context.Context, name string) (string, error) {
	v, err := s.Secrets.Get(ctx, name)
	if err == nil {
		s.r.addResolved(v)
	}
	return v, err
}

// Redact returns s with every secret that the agent knows replaced, as the
// kernel does for each ledger event. Use it before text goes to a log line.
func (a *Agent) Redact(s string) string { return a.redact.text(s) }

// AddSecret makes the agent replace v in every later ledger event and in
// Redact. A package that makes a secret of its own, such as the password
// of a process that it starts, adds it before the secret can reach an
// event or a log line. A value shorter than 8 bytes is left out, because
// it matches too much text.
func (a *Agent) AddSecret(v string) { a.redact.addOwn(v) }

// RemoveSecret takes out a value that AddSecret added, once it is no
// longer a secret, such as the secret of a process that ended, so the
// values of the redactor do not grow without end. A value that fw resolved
// through Secrets stays.
func (a *Agent) RemoveSecret(v string) { a.redact.removeOwn(v) }
