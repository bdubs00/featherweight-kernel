package fw

// This file is the contract of the fleet (docs/ARCHITECTURE.md section
// 8.6). A fleet is a set of fw processes that share one bus and one fleet
// subject. Each process publishes a record of itself and of its runs, and
// any process serves one view of the whole fleet. A request about a run of
// another process goes to that process with the credential of the caller.
// The default implementation lives in the package fleet and registers
// itself with RegisterFleet.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// FleetConfig configures the fleet. An empty Subject turns the fleet off.
//
// Name is the name of this member. The default is "<atespace>/<task>"
// under ax, else "<host>:<port>". URL is the address where other members
// reach this one: an http or https URL, or "ax:<atespace>/<task>" for an
// ax Task, which the atenet router at Config.AX.Router reaches. An empty
// URL makes this member read-only to the others. Forward lists globs over
// the http and https URLs that can receive the credential of a caller; an
// ax address always can. In such a glob, "*" matches any text without a
// slash, and the glob matches the scheme, the host, and the path of the
// URL. A URL with user information, a query, or a fragment never receives
// a credential. Interval is the time between two heartbeats, and
// Expire is the age after which the view ignores a record.
type FleetConfig struct {
	Subject  string        `yaml:"subject,omitempty" json:"subject,omitempty"`
	Name     string        `yaml:"name,omitempty" json:"name,omitempty"`
	URL      string        `yaml:"url,omitempty" json:"url,omitempty"`
	Forward  []string      `yaml:"forward,omitempty" json:"forward,omitempty"`
	Interval time.Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
	Expire   time.Duration `yaml:"expire,omitempty" json:"expire,omitempty"`
}

// Defaults and limits of the fleet.
const (
	DefaultFleetInterval = 30 * time.Second
	DefaultFleetExpire   = 7 * 24 * time.Hour
	FleetVersion         = 1 // the version of the record shape

	// FleetFinishedRuns is how many finished runs a record holds, newest first.
	FleetFinishedRuns = 50
	// FleetTextLimit is the size in bytes to which a record cuts the input,
	// the output, and the error of a run.
	FleetTextLimit = 2 << 10
	// FleetPendingInputLimit is the largest input of a pending tool call
	// that a record holds. A larger input is left out.
	FleetPendingInputLimit = 4 << 10
	// FleetRecordLimit is the largest record in bytes. The oldest finished
	// runs are left out first.
	FleetRecordLimit = 512 << 10
	// FleetQuietAfter is how many intervals without a message make a
	// member quiet.
	FleetQuietAfter = 3
)

// HeaderFleetVia names the view that forwarded a request. A member never
// forwards a request that carries it, so a stale record cannot cause a loop.
const HeaderFleetVia = "Fw-Fleet-Via"

// Bus event types of the fleet. The data of EvFleetRecord is a FleetRecord,
// and the data of EvFleetAlive is a FleetAlive.
const (
	EvFleetRecord = "fw.fleet.record"
	EvFleetAlive  = "fw.fleet.alive"
)

// Authorization modes of a member.
const (
	AccessModeRules    = "rules"    // the access rules of section 13.2
	AccessModeExternal = "external" // access.external decides
	AccessModeCustom   = "custom"   // an Authz from WithAuthz decides
)

// States of a member in the view.
const (
	MemberLive    = "live"    // published within FleetQuietAfter intervals
	MemberQuiet   = "quiet"   // did not; under ax usually a Task that sleeps
	MemberStopped = "stopped" // sent a last record at shutdown
)

// FleetRecord is what a member publishes about itself. Runs holds every run
// that is not finished and the newest finished runs, cut to size (see the
// Fleet* limits). Omitted counts the finished runs that the record leaves
// out. Interval is the fleet.interval of the member, so a view judges its
// heartbeats by its own setting.
type FleetRecord struct {
	Version   int           `json:"version"`
	Name      string        `json:"name"`
	URL       string        `json:"url,omitempty"`
	Atespace  string        `json:"atespace,omitempty"`
	Interval  time.Duration `json:"interval,omitempty"`
	Access    FleetAccess   `json:"access"`
	Agents    []FleetAgent  `json:"agents"`
	Runs      []*Run        `json:"runs"`
	Omitted   int           `json:"omitted,omitempty"`
	Stopped   bool          `json:"stopped,omitempty"`
	UpdatedAt time.Time     `json:"updatedAt"`
}

// FleetAccess is the authorization of a member. Default and Admins are set
// in the rules mode only.
type FleetAccess struct {
	Mode    string   `json:"mode"`
	Default string   `json:"default,omitempty"`
	Admins  []string `json:"admins,omitempty"`
}

// FleetAgent is one agent profile of a member. Role is the role of the
// caller on the agent: "owner", "operator", or "viewer". It is empty in a
// record, and a view sets it for each caller.
type FleetAgent struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Access      *Access  `json:"access,omitempty"`
	Teams       []string `json:"teams,omitempty"` // the teams of the agent (section 13.6)
	Role        string   `json:"role,omitempty"`
}

// FleetAlive is the heartbeat of a member.
type FleetAlive struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

// FleetMember is one member as the view shows it to a caller. Agents holds
// the agents that the caller can view. Active and InputRequired count the
// runs that the caller can view. CanForward reports whether this process
// can send a request to the member.
type FleetMember struct {
	Name          string       `json:"name"`
	URL           string       `json:"url,omitempty"`
	Self          bool         `json:"self,omitempty"`
	State         string       `json:"state"`
	LastSeen      time.Time    `json:"lastSeen"`
	CanForward    bool         `json:"canForward"`
	AccessMode    string       `json:"accessMode"`
	Agents        []FleetAgent `json:"agents"`
	Active        int          `json:"active"`
	InputRequired int          `json:"inputRequired"`
}

// Fleet is the view of the other members of the fleet.
//
// Members lists the members that the principal in ctx can see, this
// process first. A member is visible when the caller can view one of its
// agents or one of its runs. Runs lists the runs of the other members that
// the principal can view, newest first, each with Member set. Run returns
// one of them, or ErrRunNotFound when the view does not know the run or
// the principal cannot view it. Every decision follows the rules of the
// owner (section 8.6).
//
// Forward sends req to a member and returns its response, which the caller
// closes. req carries the method, the path under /v1 with its query, the
// body, and the headers Accept, Content-Type, Idempotency-Key, and
// Last-Event-ID. Forward sets the address, the credential from
// CredentialFrom(ctx), and HeaderFleetVia. It returns ErrFleetNoRoute when
// the member is unknown, has no URL, or has a URL outside fleet.forward,
// and ErrFleetNoCredential when ctx carries no bearer credential.
type Fleet interface {
	Name() string
	Members(ctx context.Context) ([]FleetMember, error)
	Runs(ctx context.Context) ([]*Run, error)
	Run(ctx context.Context, id string) (*Run, error)
	Forward(ctx context.Context, member string, req *http.Request) (*http.Response, error)
}

// Errors of the fleet. A surface answers ErrFleetNoRoute with 502 and
// ErrFleetNoCredential with 403: the caller is authenticated here, but it
// carries no credential that the owner can check.
var (
	ErrFleetNoRoute      = errors.New("fw: no route to the fleet member")
	ErrFleetNoCredential = errors.New("fw: the request carries no bearer credential to forward")
)

// Retainer is a Bus that keeps the last event of each subject.
//
// Retain replaces the kept event of a subject and delivers the event to
// every watcher. A retained event never enters the stream of Publish, so a
// heartbeat does not fill the bus. Watch first delivers the kept event of
// each subject that matches the pattern, then each new one, until stop.
// The pattern is a subject with the wildcards of SubjectMatch. The handler
// runs on one goroutine per watch.
type Retainer interface {
	Retain(ctx context.Context, subject string, e Event) error
	Watch(ctx context.Context, pattern string, h func(Event) error) (stop func(), err error)
}

// FleetToken turns a member name into one subject token: each character
// other than a letter, a digit, "-", or "_" becomes "_".
func FleetToken(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// FleetRecordSubject is the subject of the record of a member.
func FleetRecordSubject(prefix, name string) string {
	return prefix + ".members." + FleetToken(name)
}

// FleetAliveSubject is the subject of the heartbeat of a member.
func FleetAliveSubject(prefix, name string) string {
	return prefix + ".alive." + FleetToken(name)
}

// FleetCtor builds the fleet of an agent. New calls it after the kernel
// starts, when Config.Fleet.Subject is set and no WithFleet option gives a
// fleet. A fleet that implements io.Closer is closed at shutdown, before
// the bus.
type FleetCtor func(ctx context.Context, a *Agent) (Fleet, error)

var fleetCtor = struct {
	mu sync.RWMutex
	c  FleetCtor
}{}

// RegisterFleet registers the fleet driver. The package fleet calls it.
func RegisterFleet(c FleetCtor) {
	fleetCtor.mu.Lock()
	defer fleetCtor.mu.Unlock()
	fleetCtor.c = c
}

func registeredFleet() FleetCtor {
	fleetCtor.mu.RLock()
	defer fleetCtor.mu.RUnlock()
	return fleetCtor.c
}

// WithFleet uses a fleet implementation instead of the registered driver.
func WithFleet(f Fleet) Option { return func(o *options) { o.fleet = f } }

// Fleet returns the fleet, or nil when the fleet is off.
func (a *Agent) Fleet() Fleet { return a.fleet }

// AccessMode names how this process authorizes: AccessModeRules,
// AccessModeExternal, or AccessModeCustom.
func (a *Agent) AccessMode() string { return a.accessMode }

// openFleet builds the fleet after the kernel starts.
func (a *Agent) openFleet(ctx context.Context) error {
	if a.fleet != nil || strings.TrimSpace(a.cfg.Fleet.Subject) == "" {
		return nil
	}
	c := registeredFleet()
	if c == nil {
		a.log.Warn("fleet.subject is set, but no fleet driver is registered; import the package fleet", "subject", a.cfg.Fleet.Subject)
		return nil
	}
	f, err := c(ctx, a)
	if err != nil {
		return err
	}
	a.fleet = f
	return nil
}

type credentialKey struct{}

// WithCredential stores the bearer credential of a request in the context:
// the value of its Authorization header. The fleet forwards it to the
// member that owns a run. RequireAuth sets it. It never enters a ledger
// event or a log line.
func WithCredential(ctx context.Context, authorization string) context.Context {
	return context.WithValue(ctx, credentialKey{}, authorization)
}

// CredentialFrom returns the bearer credential in the context, or "".
func CredentialFrom(ctx context.Context) string {
	s, _ := ctx.Value(credentialKey{}).(string)
	return s
}

// bearerHeader returns the Authorization header of r when it holds a bearer
// token, else "".
func bearerHeader(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(tok) == "" {
		return ""
	}
	return h
}
