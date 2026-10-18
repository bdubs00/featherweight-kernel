package openai

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Timeouts of the HTTP client that no option changes.
const (
	dialTimeout         = 10 * time.Second
	tlsHandshakeTimeout = 10 * time.Second
	maxHeaderWait       = 10 * time.Minute // the response header wait when the idle check is off
	errBodyLimit        = 64 << 10
	userAgent           = "featherweight-kernel"
)

// Sentinel errors of a call.
var (
	// errStalled is the cause of the request context when the idle timer
	// fires. The message that the user sees names the idle timeout.
	errStalled = errors.New("the stream stalled")

	// errTooLarge ends a response that grows past the byte cap.
	errTooLarge = fmt.Errorf("the response is larger than %d MB", MaxResponseBytes>>20)
)

// timeoutError is the cause of the request context when the configured
// timeout fires. It unwraps to context.DeadlineExceeded.
type timeoutError struct {
	limit time.Duration
}

func (e *timeoutError) Error() string {
	return fmt.Sprintf("the call took longer than %v (providers.openai.timeout)", e.limit)
}

func (e *timeoutError) Unwrap() error { return context.DeadlineExceeded }

// stallError is the error of a stream that sent no byte for the idle
// timeout. It unwraps to errStalled.
type stallError struct {
	idle time.Duration
}

func (e *stallError) Error() string {
	return fmt.Sprintf("no bytes from the server for %v; raise providers.openai.options.idleTimeout for a slow server", e.idle)
}

func (e *stallError) Unwrap() error { return errStalled }

// APIError is an error status from the server.
type APIError struct {
	StatusCode int
	Message    string // the error message of the body, or the status text
	Type       string // the error type of the body, when it names one
	Code       string // the error code of the body, when it names one, such as model_not_found
	Hint       string // what to check, when the adapter knows
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("HTTP %d %s", e.StatusCode, http.StatusText(e.StatusCode))
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Hint != "" {
		s += " (" + e.Hint + ")"
	}
	return s
}

// aboutModel reports whether a 404 names an unknown model rather than an
// unknown path.
func (e *APIError) aboutModel() bool {
	return e.Code == "model_not_found" || strings.Contains(strings.ToLower(e.Message), "model")
}

// newTransport returns the transport of the client: dial, TLS handshake,
// and response header timeouts, the proxy of the environment, and the TLS
// settings when there are any. The response header wait is the idle
// timeout, because a server that sends no header is a server that sends no
// byte.
func newTransport(tc *tls.Config, idle time.Duration) *http.Transport {
	headerWait := maxHeaderWait
	if idle > 0 {
		headerWait = idle
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: headerWait,
		TLSClientConfig:       tc,
	}
}

// loadTLS builds the TLS settings of the provider. It returns nil when the
// entry sets no TLS field, so the client uses the system roots.
func loadTLS(c fw.TLSConfig) (*tls.Config, error) {
	if c == (fw.TLSConfig{}) {
		return nil, nil
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CA != "" {
		pem, err := os.ReadFile(c.CA)
		if err != nil {
			return nil, fmt.Errorf("openai: read tls.ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("openai: tls.ca %s holds no PEM certificate", c.CA)
		}
		tc.RootCAs = pool
	}
	if (c.Cert == "") != (c.Key == "") {
		return nil, errors.New("openai: tls.cert and tls.key go together; set both or neither")
	}
	if c.Cert != "" {
		cert, err := tls.LoadX509KeyPair(c.Cert, c.Key)
		if err != nil {
			return nil, fmt.Errorf("openai: load tls.cert and tls.key: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	tc.InsecureSkipVerify = c.Insecure //nolint:gosec // development only, and New logs a warning
	return tc, nil
}

// send sends one request and returns a 2xx response with its body open. It
// retries a 429 and a 5xx up to maxRetries times, and a connection error
// that happened before any byte of the answer arrived up to maxNetRetries
// times. Every error carries the openai prefix, the method, and the
// endpoint without its query.
func (m *Model) send(ctx context.Context, method string, u *url.URL, body []byte) (*http.Response, error) {
	where := clean(u)
	for attempt := 0; ; attempt++ {
		resp, err := m.do(ctx, method, u, body)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		var (
			retry bool
			wait  = m.delay(attempt)
			limit = maxRetries
		)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("openai: %s %s: %w", method, where, cause(ctx))
			}
			err = cleanURLError(err, where)
			retry, limit = retryableNetErr(err), maxNetRetries
		} else {
			apiErr := m.apiError(resp)
			retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			resp.Body.Close()
			err = apiErr
			retry = retryable(resp.StatusCode)
			if hasRetryAfter {
				wait = min(retryAfter, maxWait)
			}
		}
		if !retry || attempt >= limit {
			return nil, fmt.Errorf("openai: %s %s: %w", method, where, err)
		}
		m.logger(ctx).Warn("openai: retrying the request",
			"method", method, "attempt", attempt+1, "wait", wait, "reason", err.Error())
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("openai: %s %s: %w (after %v)", method, where, cause(ctx), err)
		case <-t.C:
		}
	}
}

// do sends one request with the headers of the adapter. A configured header
// replaces the header of the same name that the adapter sends.
func (m *Model) do(ctx context.Context, method string, u *url.URL, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	hr, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		hr.Header.Set("Content-Type", "application/json")
		hr.Header.Set("Accept", "text/event-stream")
	} else {
		hr.Header.Set("Accept", "application/json")
	}
	hr.Header.Set("User-Agent", userAgent)
	if m.key != "" {
		v := m.key
		if strings.EqualFold(m.auth, "Authorization") {
			v = "Bearer " + v
		}
		hr.Header.Set(m.auth, v)
	}
	for k, vs := range m.headers {
		hr.Header.Del(k)
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	return m.client.Do(hr)
}

// apiError reads the error of a response and adds the hint that fits the
// status: the secret that holds the key on 401 and 403, and the base URL on
// a 404 that is not about a model.
func (m *Model) apiError(resp *http.Response) *APIError {
	e := readAPIError(resp, m.key)
	switch e.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		if m.key == "" {
			e.Hint = "no API key is set; put it in the secret " + m.keyName
		} else {
			e.Hint = "the API key comes from the secret " + m.keyName
		}
	case http.StatusNotFound:
		if !e.aboutModel() {
			e.Hint = "no such path; check providers.openai.baseUrl, which usually ends in /v1"
		}
	}
	return e
}

// guard wraps a response body in the byte cap and the idle timer. onStall
// runs when no byte arrived for the idle timeout; it cancels the request,
// which ends the blocked read.
func (m *Model) guard(body io.Reader, onStall func()) *guardedReader {
	g := &guardedReader{r: body, left: m.maxBytes + 1, idle: m.idle}
	if m.idle > 0 {
		g.timer = time.AfterFunc(m.idle, onStall)
	}
	return g
}

// guardedReader counts the bytes of a response and resets the idle timer on
// every read that brought bytes.
type guardedReader struct {
	r     io.Reader
	left  int64
	idle  time.Duration
	timer *time.Timer
}

func (g *guardedReader) Read(p []byte) (int, error) {
	if g.left <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > g.left {
		p = p[:g.left]
	}
	n, err := g.r.Read(p)
	g.left -= int64(n)
	if n > 0 && g.timer != nil {
		g.timer.Reset(g.idle)
	}
	return n, err
}

// stop ends the idle timer.
func (g *guardedReader) stop() {
	if g.timer != nil {
		g.timer.Stop()
	}
}

// explain maps a read error to its cause when the request context ended: a
// stalled stream, the configured timeout, or the context of the caller.
func (m *Model) explain(call context.Context, err error) error {
	if call.Err() == nil {
		return err
	}
	c := cause(call)
	if errors.Is(c, errStalled) {
		return &stallError{idle: m.idle}
	}
	return c
}

// cause returns the cause of a done context: the value given to the cancel
// function, or the plain context error.
func cause(ctx context.Context) error {
	if c := context.Cause(ctx); c != nil {
		return c
	}
	return ctx.Err()
}

// clean returns an endpoint without the query and the user information, for
// error messages and logs. A query can hold a key.
func clean(u *url.URL) string {
	c := *u
	c.User = nil
	c.RawQuery = ""
	return c.String()
}

// delay returns the exponential backoff with jitter before retry attempt+1.
func (m *Model) delay(attempt int) time.Duration {
	d := min(m.backoff<<attempt, maxBackoff)
	return d + time.Duration(rand.Int64N(int64(d)/4+1))
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// retryableNetErr reports whether a request failed before any byte of the
// answer arrived in a way that a retry can fix: a refused or reset
// connection, an early EOF, a dial timeout, or a name lookup that failed for
// a reason other than an unknown name. A certificate error is permanent. A
// wait for the response headers that timed out is not retried, because the
// server received the request.
func retryableNetErr(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var (
		verify  *tls.CertificateVerificationError
		unknown x509.UnknownAuthorityError
		host    x509.HostnameError
		invalid x509.CertificateInvalidError
		record  tls.RecordHeaderError
		dns     *net.DNSError
		op      *net.OpError
		ne      net.Error
	)
	switch {
	case errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &host), errors.As(err, &invalid), errors.As(err, &record):
		return false
	case errors.As(err, &dns):
		return !dns.IsNotFound
	case errors.As(err, &op):
		return op.Op == "dial" || op.Op == "read" || op.Op == "write"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	case errors.As(err, &ne) && ne.Timeout():
		// A dial or a TLS handshake that timed out never reached the
		// server. A wait for the response headers did.
		return !strings.Contains(err.Error(), "awaiting response headers")
	}
	// The server closed a kept-alive connection as the request went out.
	// net/http retries that for idempotent requests only.
	return strings.Contains(err.Error(), "server closed idle connection")
}

// parseRetryAfter reads a Retry-After value in seconds or as an HTTP date.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil && s >= 0 {
		return time.Duration(s * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

// cleanURLError replaces the URL in a *url.Error, which holds the query, with
// the endpoint without it.
func cleanURLError(err error, where string) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return &url.Error{Op: ue.Op, URL: where, Err: ue.Err}
	}
	return err
}

// readAPIError reads the error message from the body of an error response.
// It understands {"error": {"message": ...}}, {"error": "..."},
// {"message": "..."}, and {"detail": "..."}, and falls back to the start of
// the body. The key never appears in the message.
func readAPIError(resp *http.Response, key string) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
	e := &APIError{StatusCode: resp.StatusCode}
	var doc struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(body, &doc) == nil {
		var obj struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
		}
		var s string
		switch {
		case json.Unmarshal(doc.Error, &obj) == nil && obj.Message != "":
			e.Message, e.Type, e.Code = obj.Message, obj.Type, codeOf(obj.Code)
		case json.Unmarshal(doc.Error, &s) == nil && s != "":
			e.Message = s
		case doc.Message != "":
			e.Message, e.Code = doc.Message, codeOf(doc.Code)
		case json.Unmarshal(doc.Detail, &s) == nil && s != "":
			e.Message = s
		}
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
		if len(e.Message) > 512 {
			e.Message = e.Message[:512] + "..."
		}
	}
	if key != "" {
		e.Message = strings.ReplaceAll(e.Message, key, "[redacted]")
	}
	return e
}

// codeOf reads an error code that is a string or a number.
func codeOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}
