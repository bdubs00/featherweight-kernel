// Package axmeta serves the ax metadata endpoints on a loopback listener, as
// the stock runner does since ax commit 22954f6. See docs/ARCHITECTURE.md
// sections 11.1 and 11.3.
//
// The endpoints carry no authentication. Only a process inside the sandbox
// reaches them, through the URL in AX_METADATA_URL. They serve the manifests
// that ax.Stash recorded, with the values of secret spec.env entries replaced
// by "***".
package axmeta

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"gopkg.in/yaml.v3"
)

// Paths of the endpoints.
const (
	PathTask       = "/metadata/v1alpha1/ax/task"
	PathWorkspaces = "/metadata/v1alpha1/ax/workspaces"
)

// DefaultPort is the loopback port of the endpoints. It is the port of the
// stock runner.
const DefaultPort = 8099

// Handler returns the handler of the two endpoints.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathTask, serveTask)
	mux.HandleFunc("GET "+PathWorkspaces, serveWorkspaces)
	return mux
}

// Server serves the endpoints on a loopback listener.
type Server struct {
	ln  net.Listener
	srv *http.Server
}

// Listen serves the endpoints on 127.0.0.1 and the port. Port 0 picks a free
// port, and URL gives the result. The server answers until Close.
func Listen(ctx context.Context, port int) (*Server, error) {
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("axmeta: metadata port %d is out of range", port)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("axmeta: listen: %w", err)
	}
	s := &Server{ln: ln, srv: &http.Server{
		Handler:           Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("axmeta: serve", "error", err)
		}
	}()
	return s, nil
}

// URL is the base URL of the endpoints, the value of AX_METADATA_URL.
func (s *Server) URL() string { return "http://" + s.ln.Addr().String() }

// Close stops the server.
func (s *Server) Close(ctx context.Context) error {
	if err := s.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("axmeta: shutdown: %w", err)
	}
	return nil
}

func serveTask(w http.ResponseWriter, _ *http.Request) {
	task, _ := ax.Stashed()
	if task == nil {
		http.Error(w, "task metadata not available", http.StatusNotFound)
		return
	}
	writeYAML(w, ax.Redacted(task))
}

func serveWorkspaces(w http.ResponseWriter, _ *http.Request) {
	task, workspaces := ax.Stashed()
	var docs []any
	for _, b := range ax.Bindings(task, workspaces) {
		if b.Workspace != nil {
			docs = append(docs, b.Workspace)
		}
	}
	if len(docs) == 0 {
		http.Error(w, "workspace metadata not available", http.StatusNotFound)
		return
	}
	writeYAML(w, docs...)
}

// writeYAML writes the documents as one YAML stream with "---" between them.
func writeYAML(w http.ResponseWriter, docs ...any) {
	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			http.Error(w, "encode yaml", http.StatusInternalServerError)
			return
		}
	}
	if err := enc.Close(); err != nil {
		http.Error(w, "encode yaml", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write([]byte(sb.String()))
}
