package fw

// This file is the contract of connections: a person gives fw a grant to
// act for them at an outside service, and a tool call that needs the grant
// pauses until the person connects (docs/ARCHITECTURE.md section 7.11).
//
// A grant is a refresh token and what it was issued for. Only a GrantStore
// holds it. No grant, refresh token, or access token enters a ledger event,
// a log line, or an error message.
//
// The rules that keep a grant alive with every authorization server, the
// strictest included:
//
//   - A grant has one holder: the GrantStore of one process.
//   - One refresh of a grant runs at a time.
//   - The holder stores the new refresh token before it uses the new access
//     token.
//   - A grant that the server refuses is deleted, and the next call asks
//     the person to connect again. A lost grant is safe. An old copy of a
//     grant is not, because a server with refresh token rotation revokes
//     the grant when an old refresh token comes back.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors of connections.
var (
	// ErrGrantNotFound means that the store holds no grant for the key.
	ErrGrantNotFound = errors.New("fw: grant not found")
	// ErrGrantConflict means that the grant changed after the caller read
	// it, so a Put with that revision did not write.
	ErrGrantConflict = errors.New("fw: grant changed")
	// ErrConnectSkipped means that the person chose not to connect the
	// service, so the call that needed it does not run.
	ErrConnectSkipped = errors.New("fw: the person did not connect the service")
	// ErrNotConnectable means that the caller is not a person who can
	// connect a service, such as a workload or the static token.
	ErrNotConnectable = errors.New("fw: only a signed-in person can connect a service")
	// ErrConnectorNotFound means that no tool source of that name acts for
	// a person.
	ErrConnectorNotFound = errors.New("fw: no service of that name acts for a person")
)

// Grant is the authority that a person gave fw at one authorization
// server: a refresh token and what it was issued for. Subject is the
// Principal.Subject of the person in fw, which is also the principal of the
// runs that the person starts. Account is the name of the person at the
// service, for display only. Expires is zero when the server did not say
// when the refresh token ends.
type Grant struct {
	Subject      string    `json:"subject"`
	Issuer       string    `json:"issuer"`
	ClientID     string    `json:"clientId"`
	RefreshToken string    `json:"refreshToken"`
	Scopes       []string  `json:"scopes,omitempty"`
	Account      string    `json:"account,omitempty"`
	Created      time.Time `json:"created"`
	Refreshed    time.Time `json:"refreshed,omitzero"`
	Expires      time.Time `json:"expires,omitzero"`
}

// Key returns the key of the grant.
func (g Grant) Key() GrantKey {
	return GrantKey{Subject: g.Subject, Issuer: g.Issuer, ClientID: g.ClientID}
}

// LogValue keeps the refresh token out of every log line.
func (g Grant) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("subject", g.Subject),
		slog.String("issuer", g.Issuer),
		slog.String("clientId", g.ClientID),
	)
}

// String keeps the refresh token out of every formatted value.
func (g Grant) String() string {
	return fmt.Sprintf("grant{subject=%s issuer=%s clientId=%s}", g.Subject, g.Issuer, g.ClientID)
}

// GoString keeps the refresh token out of %#v.
func (g Grant) GoString() string { return g.String() }

// GrantKey names one grant: a person, an authorization server, and the
// client that the server issued the grant to.
type GrantKey struct {
	Subject  string `json:"subject"`
	Issuer   string `json:"issuer"`
	ClientID string `json:"clientId"`
}

// GrantStore keeps grants. Every revision is a number that grows with each
// write of a key. Get returns ErrGrantNotFound for a missing key. Put with
// prev 0 writes in any case. Put with prev other than 0 writes only when
// the stored revision is prev, else it returns ErrGrantConflict. Put
// returns the new revision only after the grant is durable. List returns
// the grants of one person, sorted by issuer and client.
type GrantStore interface {
	Get(ctx context.Context, key GrantKey) (Grant, uint64, error)
	Put(ctx context.Context, g Grant, prev uint64) (uint64, error)
	Delete(ctx context.Context, key GrantKey) error
	List(ctx context.Context, subject string) ([]Grant, error)
}

// GrantStoreCtor builds a grant store from the configuration.
type GrantStoreCtor func(ctx context.Context, cfg *Config, secrets Secrets) (GrantStore, error)

var grantStores = struct {
	mu sync.RWMutex
	m  map[string]GrantStoreCtor
}{m: map[string]GrantStoreCtor{}}

// RegisterGrantStore registers a grant store driver by name. The names
// file and memory are built in.
func RegisterGrantStore(name string, c GrantStoreCtor) {
	grantStores.mu.Lock()
	defer grantStores.mu.Unlock()
	grantStores.m[name] = c
}

// NewGrantStore builds the store that grants.store names: file by default.
func NewGrantStore(ctx context.Context, cfg *Config, secrets Secrets) (GrantStore, error) {
	name := strings.ToLower(strings.TrimSpace(cfg.Grants.Store))
	switch name {
	case "", GrantStoreFile:
		dir := cfg.Grants.Dir
		if dir == "" {
			home := cfg.Home
			if home == "" {
				home = DefaultHome()
			}
			dir = filepath.Join(home, "grants")
		}
		keyName := cfg.Grants.Key
		if keyName == "" {
			keyName = DefaultGrantKey
		}
		return NewFileGrantStore(dir, grantKeyLoader(secrets, keyName, filepath.Join(dir, grantKeyFile))), nil
	case GrantStoreMemory:
		return NewMemGrantStore(), nil
	}
	grantStores.mu.RLock()
	c, ok := grantStores.m[name]
	grantStores.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fw: unknown grants.store %q", cfg.Grants.Store)
	}
	return c(ctx, cfg, secrets)
}

// Names of the built-in grant stores and the default key.
const (
	GrantStoreFile   = "file"
	GrantStoreMemory = "memory"
	// DefaultGrantKey is the Secrets name of the key that encrypts the
	// grants of the file store. Without it, the store creates a random key
	// in the file grant.key beside the grants.
	DefaultGrantKey = "FW_GRANT_KEY"
	grantKeyFile    = "grant.key"
	grantKeyLabel   = "fw grant store v1"
)

// MemGrantStore keeps grants in memory. Tests use it, and a process that
// must forget every grant when it stops.
type MemGrantStore struct {
	mu   sync.Mutex
	m    map[GrantKey]Grant
	revs map[GrantKey]uint64
}

// NewMemGrantStore returns an empty in-memory grant store.
func NewMemGrantStore() *MemGrantStore {
	return &MemGrantStore{m: map[GrantKey]Grant{}, revs: map[GrantKey]uint64{}}
}

// Get implements GrantStore.
func (s *MemGrantStore) Get(_ context.Context, key GrantKey) (Grant, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.m[key]
	if !ok {
		return Grant{}, 0, ErrGrantNotFound
	}
	return cloneGrant(g), s.revs[key], nil
}

// Put implements GrantStore.
func (s *MemGrantStore) Put(_ context.Context, g Grant, prev uint64) (uint64, error) {
	key := g.Key()
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev != 0 && s.revs[key] != prev {
		return 0, ErrGrantConflict
	}
	s.revs[key]++
	s.m[key] = cloneGrant(g)
	return s.revs[key], nil
}

// Delete implements GrantStore.
func (s *MemGrantStore) Delete(_ context.Context, key GrantKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	s.revs[key]++ // a later Put with an old revision fails
	return nil
}

// List implements GrantStore.
func (s *MemGrantStore) List(_ context.Context, subject string) ([]Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Grant
	for k, g := range s.m {
		if k.Subject == subject {
			out = append(out, cloneGrant(g))
		}
	}
	sortGrants(out)
	return out, nil
}

func cloneGrant(g Grant) Grant {
	g.Scopes = slices.Clone(g.Scopes)
	return g
}

func sortGrants(gs []Grant) {
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].Issuer != gs[j].Issuer {
			return gs[i].Issuer < gs[j].Issuer
		}
		return gs[i].ClientID < gs[j].ClientID
	})
}

// FileGrantStore keeps each grant in one file, encrypted with AES-256-GCM.
// A write goes to a temporary file, which is synced and then renamed over
// the old file, and the directory is synced after. A grant is therefore
// whole and durable when Put returns. The directory and the key are
// created on first use, with modes 0700 and 0600.
//
// Under ax, the home directory is on the durable volume, and a suspend
// archives it before it stops the process. A refresh after the archive is
// lost on resume. The server then sees the old refresh token again, and a
// server with rotation revokes the grant: the person connects again.
type FileGrantStore struct {
	dir string
	key func() ([]byte, error)

	mu sync.Mutex // one write at a time, so a revision check holds
}

// NewFileGrantStore returns a store in dir. key returns the 32-byte
// encryption key. It runs on first use, not here.
func NewFileGrantStore(dir string, key func() ([]byte, error)) *FileGrantStore {
	var once sync.Once
	var k []byte
	var kerr error
	return &FileGrantStore{dir: dir, key: func() ([]byte, error) {
		once.Do(func() { k, kerr = key() })
		return k, kerr
	}}
}

// grantFile is the content of one grant file.
type grantFile struct {
	Version int    `json:"v"`
	Rev     uint64 `json:"rev"`
	Nonce   []byte `json:"nonce"`
	Data    []byte `json:"data"`
}

func (s *FileGrantStore) path(key GrantKey) string {
	return filepath.Join(s.dir, grantName(key)+".grant")
}

// grantName is the file name of a key: a hash, so no subject or issuer
// appears in a directory listing.
func grantName(key GrantKey) string {
	h := sha256.Sum256([]byte(key.Subject + "\x00" + key.Issuer + "\x00" + key.ClientID))
	return hex.EncodeToString(h[:16])
}

func (s *FileGrantStore) aead() (cipher.AEAD, error) {
	k, err := s.key()
	if err != nil {
		return nil, fmt.Errorf("fw: grant key: %w", err)
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, fmt.Errorf("fw: grant key: %w", err)
	}
	return cipher.NewGCM(b)
}

// read decrypts one grant file. A missing file is ErrGrantNotFound.
func (s *FileGrantStore) read(path string) (Grant, uint64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Grant{}, 0, ErrGrantNotFound
	}
	if err != nil {
		return Grant{}, 0, fmt.Errorf("fw: read grant: %w", err)
	}
	var f grantFile
	if err := json.Unmarshal(b, &f); err != nil || f.Version != 1 {
		return Grant{}, 0, fmt.Errorf("fw: grant file %s is not readable", filepath.Base(path))
	}
	aead, err := s.aead()
	if err != nil {
		return Grant{}, 0, err
	}
	plain, err := aead.Open(nil, f.Nonce, f.Data, []byte(filepath.Base(path)))
	if err != nil {
		return Grant{}, 0, fmt.Errorf("fw: grant file %s does not open with the grant key", filepath.Base(path))
	}
	var g Grant
	if err := json.Unmarshal(plain, &g); err != nil {
		return Grant{}, 0, fmt.Errorf("fw: grant file %s is not readable", filepath.Base(path))
	}
	return g, f.Rev, nil
}

// Get implements GrantStore.
func (s *FileGrantStore) Get(_ context.Context, key GrantKey) (Grant, uint64, error) {
	return s.read(s.path(key))
}

// Put implements GrantStore.
func (s *FileGrantStore) Put(_ context.Context, g Grant, prev uint64) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(g.Key())
	_, rev, err := s.read(path)
	switch {
	case errors.Is(err, ErrGrantNotFound):
		rev = 0
	case err != nil && prev != 0:
		return 0, err
	}
	if prev != 0 && rev != prev {
		return 0, ErrGrantConflict
	}
	aead, err := s.aead()
	if err != nil {
		return 0, err
	}
	plain, err := json.Marshal(g)
	if err != nil {
		return 0, fmt.Errorf("fw: encode grant: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return 0, fmt.Errorf("fw: grant nonce: %w", err)
	}
	f := grantFile{Version: 1, Rev: rev + 1, Nonce: nonce, Data: aead.Seal(nil, nonce, plain, []byte(filepath.Base(path)))}
	b, err := json.Marshal(f)
	if err != nil {
		return 0, fmt.Errorf("fw: encode grant: %w", err)
	}
	if err := writeDurable(s.dir, path, b); err != nil {
		return 0, fmt.Errorf("fw: write grant: %w", err)
	}
	return f.Rev, nil
}

// Delete implements GrantStore.
func (s *FileGrantStore) Delete(_ context.Context, key GrantKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(key))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fw: delete grant: %w", err)
	}
	syncDir(s.dir)
	return nil
}

// List implements GrantStore.
func (s *FileGrantStore) List(_ context.Context, subject string) ([]Grant, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fw: list grants: %w", err)
	}
	var out []Grant
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".grant") {
			continue
		}
		g, _, err := s.read(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if g.Subject == subject {
			out = append(out, g)
		}
	}
	sortGrants(out)
	return out, nil
}

// writeDurable writes b to path through a synced temporary file and a
// rename, then syncs the directory.
func writeDurable(dir, path string, b []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".grant-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op after the rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// grantKeyLoader returns the key of the file store: derived from the
// secret name when Secrets holds it, else from a random key in file, which
// it creates with mode 0600 when it is missing.
func grantKeyLoader(secrets Secrets, name, file string) func() ([]byte, error) {
	derive := func(src []byte) []byte {
		m := hmac.New(sha256.New, src)
		m.Write([]byte(grantKeyLabel))
		return m.Sum(nil)
	}
	return func() ([]byte, error) {
		if secrets != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			v, err := secrets.Get(ctx, name)
			cancel()
			if err == nil && v != "" {
				return derive([]byte(v)), nil
			}
		}
		src, err := loadOrCreateKey(file)
		if err != nil {
			return nil, err
		}
		return derive(src), nil
	}
}

// loadOrCreateKey reads 32 random bytes from path, or creates the file.
// Two processes that create it at once agree on one key, because the
// file appears whole through O_EXCL on a synced temporary file and a link.
func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".key-*")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(key); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(name, path); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	syncDir(dir)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 32 {
		return nil, fmt.Errorf("fw: key file %s is too short", path)
	}
	return b, nil
}

// Connection is the state of one service that acts for a person, as a
// surface shows it. It never carries a token. SignIn is true when the
// service trusts the sign-in issuer of fw (auth.oidc), so one connection
// covers every such service with the same client.
type Connection struct {
	Server    string    `json:"server"`
	Title     string    `json:"title"`
	Issuer    string    `json:"issuer,omitempty"`
	Connected bool      `json:"connected"`
	Account   string    `json:"account,omitempty"`
	Since     time.Time `json:"since,omitzero"`
	Refreshed time.Time `json:"refreshed,omitzero"`
	SignIn    bool      `json:"signIn,omitempty"`
}

// ConnectStart is one connection in progress. URL is the authorization
// request that the browser of the person opens. The surface keeps the
// whole value in an HttpOnly cookie that it signs, because Verifier (the
// PKCE verifier) is a secret, and gives it back to FinishConnect with the
// code from the callback. State is the state parameter of the request.
type ConnectStart struct {
	Server   string    `json:"server"`
	URL      string    `json:"url"`
	State    string    `json:"state"`
	Verifier string    `json:"verifier"`
	Nonce    string    `json:"nonce,omitempty"`
	Redirect string    `json:"redirect"`
	Expires  time.Time `json:"expires"`
}

// Connector is a tool source that acts for the person who starts a run, so
// the person connects it first. The MCP source implements it for a
// tools.mcp entry with auth.oauth.grant: user. Surfaces reach connectors
// through the Agent methods Connections, BeginConnect, FinishConnect, and
// Disconnect, which check the person and put the agent in ctx (AgentFrom),
// so a connector reaches Agent.Grants.
//
// ConnectName is the name of the service, the tools.mcp entry name, or ""
// when the source acts as the agent itself. Agent.Connectors leaves such a
// source out. Connection says whether subject has a grant. BeginConnect
// builds the authorization request with PKCE, the state, and the redirect
// address of the callback. FinishConnect exchanges the code, checks that
// the person at the sign-in issuer is subject, and stores the grant.
// Disconnect deletes the grant and revokes it at the server when the
// server offers revocation (RFC 7009).
type Connector interface {
	ConnectName() string
	Connection(ctx context.Context, subject string) (Connection, error)
	BeginConnect(ctx context.Context, subject, redirect string) (ConnectStart, error)
	FinishConnect(ctx context.Context, subject string, start ConnectStart, code string) (Connection, error)
	Disconnect(ctx context.Context, subject string) error
}

// Reasons of a ConnectRequest.
const (
	ConnectMissing = "missing" // the person never connected the service
	ConnectExpired = "expired" // the server refused the grant, so the person signs in again
)

// ConnectRequest pauses a tool call until the person of the run connects a
// service. It travels as an approval with Connect set, so the run moves to
// input-required and every surface shows it. Only the end of a connection
// allows it: FinishConnect decides every pending connect of the person for
// the services that are now connected. A person can deny it, which skips
// the service: the call fails with ErrConnectSkipped. A decision that
// allows it from a surface is refused, because a click is not a grant.
type ConnectRequest struct {
	Server string `json:"server"`
	Title  string `json:"title,omitempty"`
	Reason string `json:"reason,omitempty"` // ConnectMissing or ConnectExpired
}

// Message is the sentence that a surface shows for the request.
func (r ConnectRequest) Message() string {
	title := r.Title
	if title == "" {
		title = r.Server
	}
	if r.Reason == ConnectExpired {
		return "Sign in to " + title + " again, so the agent can go on acting as you."
	}
	return "Connect " + title + ", so the agent can act as you there."
}

// Connecter pauses a tool call until the person connects a service.
type Connecter func(ctx context.Context, req ConnectRequest) error

type connecterKey struct{}

// WithConnecter stores a connecter in the context. The kernel sets one for
// every tool call.
func WithConnecter(ctx context.Context, fn Connecter) context.Context {
	return context.WithValue(ctx, connecterKey{}, fn)
}

// RequestConnect pauses the tool call in ctx until the person of the run
// connects req.Server. It returns nil once the person connected,
// ErrConnectSkipped when the person skipped the service, and ErrNoAsker
// outside a tool call. The caller checks that the run has a person
// (RunInfo.PrincipalKind is KindUser) first, because nobody else can
// connect.
func RequestConnect(ctx context.Context, req ConnectRequest) error {
	fn, ok := ctx.Value(connecterKey{}).(Connecter)
	if !ok || fn == nil {
		return ErrNoAsker
	}
	return fn(ctx, req)
}

// Grants returns the grant store.
func (a *Agent) Grants() GrantStore { return a.grants }

// Connectors returns the tool sources that act for a person, in the order
// of the configuration.
func (a *Agent) Connectors() []Connector {
	var out []Connector
	for _, s := range a.sources {
		if c, ok := s.(Connector); ok && c.ConnectName() != "" {
			out = append(out, c)
		}
	}
	return out
}

// connectorFor returns the connector named server. Names match loosely,
// as SameServerName says.
func (a *Agent) connectorFor(server string) (Connector, error) {
	for _, c := range a.Connectors() {
		if SameServerName(c.ConnectName(), server) {
			return c, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrConnectorNotFound, server)
}

// connectSubject returns the subject of a person who can connect.
func connectSubject(p *Principal) (string, error) {
	if p == nil || p.Kind != KindUser || strings.TrimSpace(p.Subject) == "" {
		return "", ErrNotConnectable
	}
	return p.Subject, nil
}

// Connections lists the services that act for a person, with the state of
// each for p.
func (a *Agent) Connections(ctx context.Context, p *Principal) ([]Connection, error) {
	subject, err := connectSubject(p)
	if err != nil {
		return nil, err
	}
	ctx = WithAgent(ctx, a)
	var out []Connection
	for _, c := range a.Connectors() {
		conn, err := c.Connection(ctx, subject)
		if err != nil {
			return nil, err
		}
		out = append(out, conn)
	}
	return out, nil
}

// BeginConnect starts a connection of server for p. redirect is the
// absolute address of the callback route of the surface.
func (a *Agent) BeginConnect(ctx context.Context, p *Principal, server, redirect string) (ConnectStart, error) {
	subject, err := connectSubject(p)
	if err != nil {
		return ConnectStart{}, err
	}
	c, err := a.connectorFor(server)
	if err != nil {
		return ConnectStart{}, err
	}
	return c.BeginConnect(WithAgent(ctx, a), subject, redirect)
}

// FinishConnect ends a connection that BeginConnect started: it stores the
// grant, then lets every paused call of p go on whose service is now
// connected. It returns the connections of p after the change.
func (a *Agent) FinishConnect(ctx context.Context, p *Principal, start ConnectStart, code string) ([]Connection, error) {
	subject, err := connectSubject(p)
	if err != nil {
		return nil, err
	}
	c, err := a.connectorFor(start.Server)
	if err != nil {
		return nil, err
	}
	if _, err := c.FinishConnect(WithAgent(ctx, a), subject, start, code); err != nil {
		return nil, err
	}
	conns, err := a.Connections(ctx, p)
	if err != nil {
		return nil, err
	}
	connected := map[string]bool{}
	for _, cn := range conns {
		if cn.Connected {
			connected[cn.Server] = true
		}
	}
	if n := a.resumeConnects(subject, connected); n > 0 {
		a.log.Info("connection resumed paused calls", "subject", subject, "server", start.Server, "runs", n)
	}
	return conns, nil
}

// Disconnect deletes the grant of p for server.
func (a *Agent) Disconnect(ctx context.Context, p *Principal, server string) error {
	subject, err := connectSubject(p)
	if err != nil {
		return err
	}
	c, err := a.connectorFor(server)
	if err != nil {
		return err
	}
	return c.Disconnect(WithAgent(ctx, a), subject)
}

// resumeConnects allows every pending connect of subject whose service is
// in connected. The decision comes from the process, which is the only
// decider that a connect accepts.
func (a *Agent) resumeConnects(subject string, connected map[string]bool) int {
	n := 0
	for _, rs := range a.k.all() {
		rs.mu.Lock()
		p, principal := rs.run.Pending, rs.queued.Principal
		rs.mu.Unlock()
		if p == nil || p.Connect == nil || principal != subject {
			continue
		}
		ok := false
		for s := range connected {
			if SameServerName(s, p.Connect.Server) {
				ok = true
				break
			}
		}
		if !ok {
			continue
		}
		err := a.Decide(context.Background(), rs.id, ApprovalDecision{ApprovalID: p.ID, Decision: "allow", Decider: subject, Note: "connected"})
		if err == nil {
			n++
		}
	}
	return n
}

func connectTitle(r *ConnectRequest) string {
	if r.Title != "" {
		return r.Title
	}
	return r.Server
}
