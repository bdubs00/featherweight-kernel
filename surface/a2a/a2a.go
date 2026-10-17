// Package a2a is the A2A server surface of fw. It serves the agent card at
// /.well-known/agent-card.json and the JSON-RPC and REST bindings of A2A
// protocol 1.0 under /a2a. See docs/ARCHITECTURE.md section 8.3.
//
// An A2A task is an fw run, and the task id is the run id. The ledger stays
// the only record: GetTask, ListTasks, and SubscribeToTask read the runs, so
// a task survives a restart like its run does. Push notification configs are
// the exception. They live in memory and do not survive a restart.
//
// The package implements the a2asrv.RequestHandler interface itself instead
// of the SDK default handler, because the default handler keeps its own task
// store and refuses a message to a finished task, while fw reopens a finished
// run for a follow-up.
package a2a

import (
	"context"
	"fmt"
	"net/http"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// Paths of the surface.
const (
	PathCard    = a2asrv.WellKnownAgentCardPath // the public agent card
	PathJSONRPC = "/a2a/jsonrpc"                // the JSON-RPC binding
	PathREST    = "/a2a"                        // the root of the REST binding
)

const (
	keepAlive   = 15 * time.Second // SSE comment interval on a quiet stream
	pushTimeout = 30 * time.Second // limit of one push request
)

func init() {
	fw.RegisterSurface("a2a", func(*fw.Config) (fw.Surface, error) { return New(), nil })
}

// Surface is the A2A server surface.
type Surface struct {
	// AllowPrivatePush lets push notifications reach loopback, private, and
	// link-local addresses. It is off by default, as A2A section 13.2 asks,
	// so that a caller cannot point the agent at an internal service.
	AllowPrivatePush bool
}

// New returns the A2A surface with the default settings.
func New() *Surface { return &Surface{} }

// Mount implements fw.Surface. The card is public. The two bindings sit
// behind the agent Authn.
func (s *Surface) Mount(mux *http.ServeMux, a *fw.Agent) error {
	if _, err := configBase(a.Config()); err != nil {
		return err
	}
	log := a.Logger().With("surface", "a2a")
	p := &pusher{
		a:     a,
		log:   log,
		store: push.NewInMemoryStore(),
		sender: push.NewHTTPPushSender(&push.HTTPSenderConfig{
			Timeout:              pushTimeout,
			FailOnError:          true,
			AllowPrivateNetworks: s.AllowPrivatePush,
		}),
		last:  map[string]sdk.TaskState{},
		queue: map[string][]*sdk.Task{},
		busy:  map[string]bool{},
	}
	h := &handler{a: a, log: log, push: p}
	panicked := func(r any) error {
		log.Error("a2a handler panic", "panic", fmt.Sprint(r))
		return fmt.Errorf("a2a: %w", sdk.ErrInternalError)
	}
	opts := []a2asrv.TransportOption{
		a2asrv.WithTransportKeepAlive(keepAlive),
		a2asrv.WithTransportPanicHandler(panicked),
	}
	mux.Handle(PathCard, cardHandler(a))
	mux.Handle(PathJSONRPC, fw.RequireAuth(a, a2asrv.NewJSONRPCHandler(h, opts...)))
	mux.Handle(PathREST+"/", fw.RequireAuth(a, http.StripPrefix(PathREST, unavailableStatus(a2asrv.NewRESTHandler(h, opts...)))))
	return nil
}

type baseKey struct{}

// cardHandler serves the public card through the SDK card handler, which
// answers CORS preflight requests too. The interface URLs use Config.A2A.URL
// when it is set, else the scheme and Host of the request, so that a client
// that reached the card can reach the bindings at the same address.
func cardHandler(a *fw.Agent) http.Handler {
	inner := a2asrv.NewAgentCardHandler(a2asrv.AgentCardProducerFn(func(ctx context.Context) (*sdk.AgentCard, error) {
		base, _ := ctx.Value(baseKey{}).(string)
		return cardFor(a, base), nil
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := requestBase(a.Config(), r)
		inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), baseKey{}, base)))
	})
}
