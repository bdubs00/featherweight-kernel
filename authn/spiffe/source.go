package spiffe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/spiffebundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

const (
	urlRefresh   = 5 * time.Minute  // how often a bundle URL is fetched again
	urlMinHint   = 10 * time.Second // the shortest refresh hint that is honored
	fetchTimeout = 15 * time.Second // limit for one bundle URL fetch
	startGrace   = 3 * time.Second  // how long the Workload API gets before the warning
	retryMin     = 2 * time.Second  // first retry delay for a source that failed
	retryMax     = 30 * time.Second // longest retry delay for the Workload API
	maxBundle    = 1 << 20          // largest bundle document read from a URL
)

// source is the live trust bundle of one trust domain, plus the X.509-SVID
// of this workload when the Workload API is in use. It implements
// jwtbundle.Source, x509bundle.Source, and x509svid.Source, and it answers
// for its own trust domain only. A source that is not ready yet answers
// every lookup with an error, so every request is refused until the bundle
// arrives.
type source struct {
	td   spiffeid.TrustDomain
	path string // the bundle file, or ""
	url  string // the bundle URL, or ""
	log  *slog.Logger

	mu     sync.RWMutex
	bundle *spiffebundle.Bundle    // from the file or the URL
	jwt    *workloadapi.JWTSource  // from the Workload API
	x509   *workloadapi.X509Source // from the Workload API
	mtime  time.Time               // of the file at the last load
	size   int64                   // of the file at the last load
	warned bool                    // one warning about a source that fails
}

// openSource builds the source that the configuration names. bundle is a
// file path or a URL; with none, the Workload API at socket, or at
// SPIFFE_ENDPOINT_SOCKET, is used. wantJWT and wantX509 say which Workload
// API streams to open. A configuration error fails the call. A source that
// does not answer yet does not: the call logs one warning and the source
// keeps trying in the background until ctx ends.
func openSource(ctx context.Context, td spiffeid.TrustDomain, bundle, socket string, wantJWT, wantX509 bool, log *slog.Logger) (*source, error) {
	s := &source{td: td, log: log}
	bundle = strings.TrimSpace(bundle)
	switch {
	case strings.HasPrefix(bundle, "https://") || strings.HasPrefix(bundle, "http://"):
		if err := checkBundleURL(bundle); err != nil {
			return nil, err
		}
		s.url = bundle
		s.log = log.With("bundle", bundle)
		hc := &http.Client{Timeout: fetchTimeout}
		s.fetchURL(ctx, hc)
		go s.refreshURL(ctx, hc)
	case bundle != "":
		s.path = bundle
		s.log = log.With("bundle", bundle)
		s.reload()
	default:
		where := "auth.spiffe.socket"
		if socket == "" {
			where = workloadapi.SocketEnv
			socket, _ = workloadapi.GetDefaultAddress()
		}
		if socket == "" {
			return nil, errors.New("spiffe: no bundle source: set auth.spiffe.bundle to a file or a URL, or auth.spiffe.socket or SPIFFE_ENDPOINT_SOCKET to the Workload API")
		}
		if err := workloadapi.ValidateAddress(socket); err != nil {
			return nil, fmt.Errorf("spiffe: %s %q: %w", where, socket, err)
		}
		s.log = log.With("socket", socket)
		s.watchWorkload(ctx, socket, wantJWT, wantX509)
	}
	return s, nil
}

// checkBundleURL refuses a bundle over plain http from outside the machine,
// because whoever answers decides who is trusted.
func checkBundleURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("spiffe: auth.spiffe.bundle %q is not a URL", raw)
	}
	if u.Scheme == "http" {
		host := strings.ToLower(u.Hostname())
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("spiffe: auth.spiffe.bundle %s must use https", raw)
		}
	}
	return nil
}

// GetJWTBundleForTrustDomain implements jwtbundle.Source.
func (s *source) GetJWTBundleForTrustDomain(td spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	if td != s.td {
		return nil, fmt.Errorf("spiffe: trust domain %s is not %s", td, s.td)
	}
	s.reload()
	s.mu.RLock()
	b, j := s.bundle, s.jwt
	s.mu.RUnlock()
	switch {
	case b != nil:
		return b.GetJWTBundleForTrustDomain(td)
	case j != nil:
		return j.GetJWTBundleForTrustDomain(td)
	}
	return nil, s.notReady()
}

// GetX509BundleForTrustDomain implements x509bundle.Source.
func (s *source) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	if td != s.td {
		return nil, fmt.Errorf("spiffe: trust domain %s is not %s", td, s.td)
	}
	s.reload()
	s.mu.RLock()
	b, x := s.bundle, s.x509
	s.mu.RUnlock()
	switch {
	case b != nil:
		return b.GetX509BundleForTrustDomain(td)
	case x != nil:
		return x.GetX509BundleForTrustDomain(td)
	}
	return nil, s.notReady()
}

// GetX509SVID implements x509svid.Source. Only the Workload API gives a
// certificate for this workload.
func (s *source) GetX509SVID() (*x509svid.SVID, error) {
	s.mu.RLock()
	x := s.x509
	s.mu.RUnlock()
	if x == nil {
		return nil, s.notReady()
	}
	return x.GetX509SVID()
}

// notReady names what the source waits for.
func (s *source) notReady() error {
	switch {
	case s.path != "":
		return fmt.Errorf("spiffe: bundle file %s is not loaded yet", s.path)
	case s.url != "":
		return fmt.Errorf("spiffe: bundle %s is not fetched yet", s.url)
	}
	return errors.New("spiffe: the Workload API has not answered yet")
}

// setBundle installs a bundle from the file or the URL.
func (s *source) setBundle(b *spiffebundle.Bundle, where string) {
	s.mu.Lock()
	s.bundle = b
	s.warned = false
	s.mu.Unlock()
	s.log.Info("spiffe bundle loaded", "from", where,
		"x509Authorities", len(b.X509Authorities()), "jwtAuthorities", len(b.JWTAuthorities()))
}

// warnOnce logs a warning about a failing source once, until the source
// works again.
func (s *source) warnOnce(msg string, err error) {
	s.mu.Lock()
	first := !s.warned
	s.warned = true
	s.mu.Unlock()
	if first {
		s.log.Warn(msg, "error", err)
	} else {
		s.log.Debug(msg, "error", err)
	}
}

// reload reads the bundle file again when its modification time or size
// changed since the last load. A file that is missing or not valid keeps
// the last bundle, so a rename in progress does not refuse requests.
func (s *source) reload() {
	if s.path == "" {
		return
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		s.warnOnce("spiffe bundle file is not readable; requests are refused until it is", err)
		return
	}
	s.mu.RLock()
	same := s.bundle != nil && fi.ModTime().Equal(s.mtime) && fi.Size() == s.size
	s.mu.RUnlock()
	if same {
		return
	}
	s.mu.Lock()
	if s.bundle != nil && fi.ModTime().Equal(s.mtime) && fi.Size() == s.size {
		// Another request loaded this version.
		s.mu.Unlock()
		return
	}
	s.mtime, s.size = fi.ModTime(), fi.Size()
	s.mu.Unlock()
	b, err := spiffebundle.Load(s.td, s.path)
	if err != nil {
		s.warnOnce("spiffe bundle file is not valid; keeping the last bundle", err)
		return
	}
	s.setBundle(b, s.path)
}

// fetchURL fetches the bundle URL once and installs the result.
func (s *source) fetchURL(ctx context.Context, hc *http.Client) bool {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		s.warnOnce("spiffe bundle URL is not valid", err)
		return false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		s.warnOnce("spiffe bundle URL is not reachable; requests are refused until it is", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.warnOnce("spiffe bundle URL did not answer 200", fmt.Errorf("GET %s: %s", req.URL.Redacted(), resp.Status))
		return false
	}
	b, err := spiffebundle.Read(s.td, io.LimitReader(resp.Body, maxBundle))
	if err != nil {
		s.warnOnce("spiffe bundle URL did not give a valid bundle", err)
		return false
	}
	s.setBundle(b, s.url)
	return true
}

// refreshURL fetches the bundle URL every urlRefresh, or sooner when the
// bundle gives a shorter refresh hint. After a failure it retries with a
// growing delay. It returns when ctx ends.
func (s *source) refreshURL(ctx context.Context, hc *http.Client) {
	delay := retryMin
	for {
		s.mu.RLock()
		b := s.bundle
		s.mu.RUnlock()
		wait := delay
		if b != nil {
			wait = urlRefresh
			if hint, ok := b.RefreshHint(); ok && hint > 0 && hint < urlRefresh {
				wait = max(hint, urlMinHint)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if s.fetchURL(ctx, hc) {
			delay = retryMin
		} else {
			delay = min(delay*2, urlRefresh)
		}
	}
}

// watchWorkload opens the Workload API streams in the background. The
// go-spiffe client retries on its own until the socket answers, and this
// loop retries when the client gives up, until ctx ends.
func (s *source) watchWorkload(ctx context.Context, socket string, wantJWT, wantX509 bool) {
	opts := workloadapi.WithClientOptions(workloadapi.WithAddr(socket), workloadapi.WithLogger(quietLogger{s.log}))
	var pending sync.WaitGroup
	if wantJWT {
		pending.Add(1)
		go func() {
			defer pending.Done()
			retry(ctx, s.log, "JWT bundles", func() error {
				src, err := workloadapi.NewJWTSource(ctx, opts)
				if err != nil {
					return err
				}
				s.mu.Lock()
				s.jwt = src
				s.mu.Unlock()
				context.AfterFunc(ctx, func() { _ = src.Close() })
				return nil
			})
		}()
	}
	if wantX509 {
		pending.Add(1)
		go func() {
			defer pending.Done()
			retry(ctx, s.log, "X.509 context", func() error {
				src, err := workloadapi.NewX509Source(ctx, opts)
				if err != nil {
					return err
				}
				s.mu.Lock()
				s.x509 = src
				s.mu.Unlock()
				context.AfterFunc(ctx, func() { _ = src.Close() })
				return nil
			})
		}()
	}
	ready := make(chan struct{})
	go func() {
		pending.Wait()
		close(ready)
	}()
	go func() {
		select {
		case <-ready:
		case <-ctx.Done():
			return
		case <-time.After(startGrace):
			s.log.Warn("spiffe Workload API has not answered; requests are refused until it does")
			<-ready
		}
		if ctx.Err() == nil {
			s.log.Info("spiffe Workload API connected")
		}
	}()
}

// retry runs open until it succeeds or ctx ends, with a growing delay.
func retry(ctx context.Context, log *slog.Logger, what string, open func() error) {
	delay := retryMin
	for {
		err := open()
		if err == nil || ctx.Err() != nil {
			return
		}
		log.Debug("spiffe Workload API stream failed; retrying", "stream", what, "error", err, "in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, retryMax)
	}
}

// quietLogger sends the logs of the go-spiffe client to slog at debug
// level. The source logs its own one warning when the Workload API does not
// answer, so a client that retries does not flood the log.
type quietLogger struct{ log *slog.Logger }

func (q quietLogger) Debugf(f string, a ...any) { q.log.Debug(fmt.Sprintf(f, a...)) }
func (q quietLogger) Infof(f string, a ...any)  { q.log.Debug(fmt.Sprintf(f, a...)) }
func (q quietLogger) Warnf(f string, a ...any)  { q.log.Debug(fmt.Sprintf(f, a...)) }
func (q quietLogger) Errorf(f string, a ...any) { q.log.Debug(fmt.Sprintf(f, a...)) }
