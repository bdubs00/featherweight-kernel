package spiffe

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// refreshMargin is how long before expiry a JWT-SVID is replaced.
const refreshMargin = 30 * time.Second

// TokenSource gives the JWT-SVID of this workload for one audience. It
// fetches the token from the Workload API on first use and keeps it until
// shortly before it expires. tools/mcp holds one per server entry with
// auth.spiffe, and sends the token as the bearer token of every request.
// The zero value is not usable; use NewTokenSource.
type TokenSource struct {
	// Audience is the audience of every token.
	Audience string
	// Socket is the Workload API address. Empty reads SPIFFE_ENDPOINT_SOCKET.
	Socket string
	// Fetch replaces the Workload API when set. It returns a token and its
	// expiry. Tests set it.
	Fetch func(ctx context.Context, audience string) (string, time.Time, error)

	mu     sync.Mutex
	client *workloadapi.Client
	tok    string
	issued time.Time
	expiry time.Time
	now    func() time.Time
}

// NewTokenSource returns a TokenSource for one audience. An empty socket
// reads SPIFFE_ENDPOINT_SOCKET.
func NewTokenSource(socket, audience string) *TokenSource {
	return &TokenSource{Socket: socket, Audience: audience}
}

// Token returns a JWT-SVID that is valid for at least a short while: the
// cached one, or a new one from the Workload API.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != "" && s.fresh() {
		return s.tok, nil
	}
	fetch := s.Fetch
	if fetch == nil {
		fetch = s.fromWorkloadAPI
	}
	tok, expiry, err := fetch(ctx, s.Audience)
	if err != nil {
		return "", fmt.Errorf("spiffe: fetch JWT-SVID for audience %q: %w", s.Audience, err)
	}
	if tok == "" {
		return "", fmt.Errorf("spiffe: fetch JWT-SVID for audience %q: empty token", s.Audience)
	}
	s.tok, s.issued, s.expiry = tok, s.clock(), expiry
	return tok, nil
}

// Close drops the Workload API connection. Token works again after Close
// and opens a new one.
func (s *TokenSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.client
	s.client = nil
	s.tok = ""
	if c == nil {
		return nil
	}
	if err := c.Close(); err != nil {
		return fmt.Errorf("spiffe: close Workload API client: %w", err)
	}
	return nil
}

func (s *TokenSource) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// fresh reports whether the cached token is usable for at least the refresh
// margin, or for half of its life when its life is shorter than twice the
// margin. A token without an expiry is kept. The caller holds s.mu.
func (s *TokenSource) fresh() bool {
	if s.expiry.IsZero() {
		return true
	}
	margin := refreshMargin
	if life := s.expiry.Sub(s.issued); life < 2*margin {
		margin = life / 2
	}
	return s.clock().Add(margin).Before(s.expiry)
}

// fromWorkloadAPI fetches a JWT-SVID from the Workload API. The caller
// holds s.mu.
func (s *TokenSource) fromWorkloadAPI(ctx context.Context, audience string) (string, time.Time, error) {
	if s.client == nil {
		opts := []workloadapi.ClientOption{workloadapi.WithLogger(quietLogger{slog.Default().With("component", "spiffe")})}
		if s.Socket != "" {
			opts = append(opts, workloadapi.WithAddr(s.Socket))
		}
		c, err := workloadapi.New(ctx, opts...)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("open Workload API client: %w", err)
		}
		s.client = c
	}
	svid, err := s.client.FetchJWTSVID(ctx, jwtsvid.Params{Audience: audience})
	if err != nil {
		return "", time.Time{}, err
	}
	return svid.Marshal(), svid.Expiry, nil
}
