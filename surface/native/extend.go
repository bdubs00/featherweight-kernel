package native

// This file lets another package add routes to the native surface, on the
// same server and behind the same authentication. The workbench, for
// example, adds its page, its sign-in, and its views. An extension gets a
// Kit with the agent and the helpers of the run routes, so its routes
// answer errors as the run routes do. An extension can also set a Relay,
// which answers a request about a run that another process holds, and
// with AgentRelay about a profile that another process runs.

import (
	"net/http"
	"slices"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Limits of a page of events.
const (
	DefaultEventsLimit = defaultEventsLimit
	MaxEventsLimit     = maxEventsLimit
)

// Extension mounts more routes on the native surface. The surface calls it
// once, after it mounts the run routes.
type Extension func(k *Kit) error

var extensions struct {
	mu   sync.Mutex
	list []Extension
}

// Extend registers an extension that New mounts. A package calls it in
// init, so that a program that links the package serves its routes.
func Extend(e Extension) {
	extensions.mu.Lock()
	defer extensions.mu.Unlock()
	extensions.list = append(extensions.list, e)
}

// registered returns the registered extensions.
func registered() []Extension {
	extensions.mu.Lock()
	defer extensions.mu.Unlock()
	return slices.Clone(extensions.list)
}

// Kit is what an extension gets from the surface: the agent, a way to mount
// routes, and the helpers of the run routes.
type Kit struct {
	s   *server
	mux *http.ServeMux
}

// Agent returns the agent of the surface.
func (k *Kit) Agent() *fw.Agent { return k.s.a }

// Handle mounts a route behind the authentication of the agent, as the run
// routes are.
func (k *Kit) Handle(pattern string, h http.HandlerFunc) { k.mux.Handle(pattern, k.s.protect(h)) }

// HandlePublic mounts a route that needs no credential, such as a page or
// the callback of a sign-in.
func (k *Kit) HandlePublic(pattern string, h http.HandlerFunc) { k.mux.HandleFunc(pattern, h) }

// SetSigninPath names the route that signs a person in. A 401 of the
// surface then carries it in the field signin of its problem document.
func (k *Kit) SetSigninPath(path string) { k.s.signinPath = path }

// Remote wraps a handler of a route with the path value id. When the relay
// knows the run as one of another process, the relay answers the request.
func (k *Kit) Remote(next http.HandlerFunc) http.HandlerFunc { return k.s.remote(next) }

// A Relay answers a request of the native API that names what another
// process holds: a run of another process, by the path value id of a run
// route, or a place to start a run, by the query parameter member of POST
// /v1/runs. Take reports whether it answered the request. The kernel knows
// nothing about the other processes. The fleet of the workbench registers
// a relay through SetRelay. See AgentRelay for the route of one agent.
type Relay interface {
	Take(w http.ResponseWriter, r *http.Request) bool
}

// An AgentRelay is a Relay that also answers GET /v1/agents/{name} for a
// profile that another process runs, such as the profile of a pool of
// workers of a fleet, which the process of the surface does not hold. The
// surface asks the relay of SetRelay for this interface. It calls
// TakeAgent after the authentication of the caller and before its own
// lookup, with the path value name. TakeAgent reports whether it answered
// the request. The kernel checks no right before the call, so the relay
// answers a name that the caller cannot view as the kernel does, with a
// 404. A relay that does not implement AgentRelay, or no relay, leaves the
// route to the kernel.
type AgentRelay interface {
	TakeAgent(w http.ResponseWriter, r *http.Request, name string) bool
}

// SetRelay sets the relay of the surface. The last extension to set one
// wins. A relay that also implements AgentRelay answers the route of one
// agent too.
func (k *Kit) SetRelay(rl Relay) { k.s.relay = rl }

// Start starts a run for the caller of the request. When it returns false,
// it wrote the error response.
func (k *Kit) Start(w http.ResponseWriter, r *http.Request, req fw.RunRequest) (*fw.Run, bool) {
	return k.s.start(w, r, req)
}

// Fail writes the problem document of a kernel error, with the status that
// the run routes give it.
func (k *Kit) Fail(w http.ResponseWriter, r *http.Request, err error) { k.s.fail(w, r, err) }

// Decode reads a JSON body of at most MaxBodyBytes into v. When it returns
// false, it wrote the error response.
func (k *Kit) Decode(w http.ResponseWriter, r *http.Request, v any) bool { return k.s.decode(w, r, v) }

// DecodeFiles reads a JSON body that can carry files into v, with the limit
// of MaxFilesBodyBytes. files counts the files of v after the read. When it
// returns false, it wrote the error response.
func (k *Kit) DecodeFiles(w http.ResponseWriter, r *http.Request, v any, files func() int) bool {
	return k.s.decodeFiles(w, r, v, files)
}

// WriteProblem writes an RFC 9457 problem document with its status.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	problem(w, r, status, detail)
}

// WriteJSON writes v as JSON with the status.
func WriteJSON(w http.ResponseWriter, status int, v any) { writeJSON(w, status, v) }

// Accepts reports whether the request accepts the media type.
func Accepts(r *http.Request, mediaType string) bool { return accepts(r, mediaType) }

// Cause returns the words of an error that a response can carry.
func Cause(err error) string { return cause(err) }

// FileBody is a file in a request body, as POST /v1/runs takes it.
type FileBody = fileBody

// Attachments turns the files of a request body into content blocks.
func Attachments(files []FileBody) []fw.Block { return attachments(files) }
