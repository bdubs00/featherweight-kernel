package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() { fw.RegisterTool("http", newHTTP) }

const httpSchema = `{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "The full http or https URL. It must match a glob in the allowlist."
    },
    "method": {
      "type": "string",
      "enum": ["GET", "POST"],
      "description": "The request method. The default is GET."
    },
    "body": {
      "type": "string",
      "description": "The request body, for POST."
    },
    "headers": {
      "type": "object",
      "additionalProperties": {"type": "string"},
      "description": "Extra request headers, by name."
    }
  },
  "required": ["url"],
  "additionalProperties": false
}`

const (
	httpTimeout  = 30 * time.Second
	httpMaxBody  = 1 << 20
	httpMaxGlobs = 20 // globs named in the description
)

// httpShownHeaders are the response headers that the result shows.
var httpShownHeaders = []string{"Content-Type", "Content-Length", "Location", "Retry-After", "ETag", "Last-Modified"}

// httpTool fetches URLs that match an allowlist.
type httpTool struct {
	allow  []string
	client *http.Client
}

func newHTTP(cfg *fw.Config) (fw.Tool, error) {
	t := &httpTool{}
	if cfg != nil {
		t.allow = append([]string(nil), cfg.Tools.HTTP.Allow...)
	}
	t.client = &http.Client{
		Timeout: httpTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if !t.allowed(req.URL.String()) {
				return fmt.Errorf("the redirect to %s is not on the allowlist (tools.http.allow)", req.URL.Redacted())
			}
			return nil
		},
	}
	return t, nil
}

// Spec implements fw.Tool.
func (t *httpTool) Spec() fw.ToolSpec {
	desc := "Fetch a URL with GET or POST and return the status, the main headers, and the body text. " +
		"The body is cut at the output limit of the configuration. " +
		"Each request has a limit of 30 seconds. Only URLs that match the allowlist in tools.http.allow work. "
	switch {
	case len(t.allow) == 0:
		desc += "The allowlist is empty, so every URL fails."
	case len(t.allow) <= httpMaxGlobs:
		desc += "Allowed URL globs: " + strings.Join(t.allow, ", ") + "."
	default:
		desc += "Allowed URL globs include: " + strings.Join(t.allow[:httpMaxGlobs], ", ") + "."
	}
	s := newSpec("http", desc, httpSchema)
	s.ReadOnly = true
	s.Idempotent = true
	s.ParallelSafe = true
	return s
}

// allowed reports whether u matches a glob. A glob matches the full URL with
// path.Match, and a final "*" also matches any suffix, slashes included.
func (t *httpTool) allowed(u string) bool {
	for _, g := range t.allow {
		if ok, _ := path.Match(g, u); ok {
			return true
		}
		prefix, ok := strings.CutSuffix(g, "*")
		if !ok {
			continue
		}
		if !strings.ContainsAny(prefix, `*?[\`) {
			if strings.HasPrefix(u, prefix) {
				return true
			}
			continue
		}
		for k := len(u); k >= 0; k-- {
			if ok, _ := path.Match(prefix, u[:k]); ok {
				return true
			}
		}
	}
	return false
}

// Call implements fw.Tool.
func (t *httpTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	var in struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Body    string            `json:"body"`
		Headers map[string]string `json:"headers"`
	}
	if err := decode(input, &in); err != nil {
		return badInput("http", err), nil
	}
	if len(t.allow) == 0 {
		return fw.ErrorResult("http: no URL is allowed. An operator adds URL globs to tools.http.allow in fw.yaml to permit requests."), nil
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost {
		return fw.ErrorResult(fmt.Sprintf("http: the method %q is not supported. Use GET or POST.", in.Method)), nil
	}
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fw.ErrorResult(fmt.Sprintf("http: %q is not a full http or https URL.", in.URL)), nil
	}
	if u.User != nil {
		return fw.ErrorResult("http: a URL with user information before the host is not allowed."), nil
	}
	target := u.String()
	if !t.allowed(target) {
		return fw.ErrorResult(fmt.Sprintf("http: %s is not on the allowlist (tools.http.allow). Allowed globs: %s", target, strings.Join(t.allow, ", "))), nil
	}

	rctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(in.Body)
	}
	req, err := http.NewRequestWithContext(rctx, method, target, body)
	if err != nil {
		return fw.ErrorResult(fmt.Sprintf("http: the request cannot be built: %v", err)), nil
	}
	for k, v := range in.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "featherweight-kernel")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return canceled(ctx, "http")
		}
		return fw.ErrorResult(fmt.Sprintf("http: %s %s failed: %v", method, target, err)), nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody+1))
	if err != nil {
		return fw.ErrorResult(fmt.Sprintf("http: reading the body of %s failed: %v", target, err)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", resp.Proto, resp.Status)
	for _, h := range httpShownHeaders {
		if v := resp.Header.Get(h); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", h, v)
		}
	}
	b.WriteByte('\n')
	cut := len(data) > httpMaxBody
	if cut {
		data = data[:httpMaxBody]
	}
	switch {
	case utf8.Valid(data) || (cut && utf8.Valid(trimPartialRune(data))):
		b.Write(trimPartialRune(data))
	default:
		fmt.Fprintf(&b, "[binary body of %d bytes, not shown]", len(data))
	}
	if cut {
		b.WriteString("\n[body truncated at 1 MB]")
	}
	return fw.TextResult(b.String()), nil
}

// trimPartialRune drops an incomplete UTF-8 sequence at the end of b.
func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			break
		}
	}
	return b
}
