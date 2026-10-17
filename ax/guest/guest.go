// Package guest serves the Agent Substrate guest services that `ax ssh`
// uses, over gRPC on the shared listener of fw. See docs/ARCHITECTURE.md
// section 11.12.
//
// The services are the upstream ones from github.com/agent-substrate/env:
// the process service starts, follows, feeds, and signals processes, and
// the file system service reads and writes files under the first
// workspace. This package puts them on one gRPC server, as the stock ax
// runner does, and adds two things. Each call gets a log line, and each
// process gets AX_METADATA_URL in its environment, as the command of the
// Task does.
//
// The services carry no authentication, as the ax runner contract
// expects. A Task turns them on with spec.debug, and anyone who reaches
// port 80 of the sandbox can then run any process in it.
package guest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agent-substrate/env/guest/filesystem"
	"github.com/agent-substrate/env/guest/process"
	ateenv "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/bdubs00/featherweight-kernel/ax"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// Options configures the guest services.
type Options struct {
	// Workspace is the working directory of a process that names none, and
	// the root of every file operation. It is the first workspace of the
	// Task, which ax.Workdir returns.
	Workspace string
	// MetadataURL is the value of AX_METADATA_URL for each process that
	// does not set it. Empty means that fw adds nothing.
	MetadataURL string
	// LogDir is where the output of each process is kept. Empty means
	// /var/log/ate-jobs, or ate-jobs under the temporary directory when
	// that path cannot be written, as in the stock ax runner.
	LogDir string
	// Logger takes the log line of each call. Nil means slog.Default() at
	// the time of the call, so a later change of the default logger applies.
	Logger *slog.Logger
}

// Server holds the guest services. The methods of a nil Server do nothing,
// and Wrap returns its argument.
type Server struct {
	srv     *grpc.Server
	tracker *process.Tracker
	opts    Options
	once    sync.Once
}

// New returns the guest services for a Task that sets spec.debug. It
// returns nil for a nil Task and for a Task that does not set spec.debug.
func New(task *ax.Task, opts Options) (*Server, error) {
	if task == nil || !task.Spec.Debug {
		return nil, nil
	}
	if opts.Workspace == "" {
		opts.Workspace = ax.DefaultWorkspacePath
	}
	cfg := process.DefaultConfig(opts.LogDir)
	cfg.Workspace = opts.Workspace
	tracker, err := process.NewTracker(cfg)
	if err != nil {
		return nil, fmt.Errorf("guest: start the process tracker: %w", err)
	}
	s := &Server{tracker: tracker, opts: opts}
	s.srv = grpc.NewServer(grpc.ChainUnaryInterceptor(s.unary), grpc.ChainStreamInterceptor(s.stream))
	reflection.Register(s.srv)
	ateenv.RegisterProcessServiceServer(s.srv, process.NewService(tracker))
	ateenv.RegisterFileSystemServiceServer(s.srv, filesystem.NewService(filesystem.Config{RootDirectory: opts.Workspace}))
	return s, nil
}

// Wrap returns a handler that sends each gRPC request over HTTP/2 to the
// guest services and every other request to next.
func (s *Server) Wrap(next http.Handler) http.Handler {
	if s == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			s.srv.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Close ends every call in progress and stops the services. The processes
// that the services started keep running. Call Close before the shutdown
// of the HTTP server, because a call that follows the output of a process
// keeps its connection busy.
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.srv.Stop()
		s.tracker.Close()
	})
}

func (s *Server) logger() *slog.Logger {
	if s.opts.Logger != nil {
		return s.opts.Logger
	}
	return slog.Default()
}

func (s *Server) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if sp, ok := req.(*ateenv.StartProcessRequest); ok {
		s.addEnv(sp)
		if sp.GetCwd() == "" {
			sp.Cwd = existingDir(s.opts.Workspace)
		}
	}
	resp, err := handler(ctx, req)
	attrs := describe(req)
	if p, ok := resp.(*ateenv.Process); ok && p.GetProcessId() != "" {
		attrs = append(attrs, "process", p.GetProcessId())
	}
	s.logCall(ctx, info.FullMethod, attrs, err)
	return resp, err
}

func (s *Server) stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	rs := &recordingStream{ServerStream: ss}
	err := handler(srv, rs)
	s.logCall(ss.Context(), info.FullMethod, describe(rs.first), err)
	return err
}

// addEnv sets AX_METADATA_URL for the process when the request does not.
func (s *Server) addEnv(req *ateenv.StartProcessRequest) {
	if s.opts.MetadataURL == "" {
		return
	}
	if _, ok := req.GetEnv()[ax.EnvMetadataURL]; ok {
		return
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	req.Env[ax.EnvMetadataURL] = s.opts.MetadataURL
}

// existingDir returns dir, or its closest parent that exists. The services
// start before the boot creates the first workspace, and a process that
// names no working directory still starts during that time.
func existingDir(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
		if d == filepath.Dir(d) {
			return d
		}
	}
}

// loud names the calls that change the sandbox or read a file. They log at
// the info level. The other calls log at the debug level.
var loud = map[string]bool{
	"StartProcess":  true,
	"SignalProcess": true,
	"ReadFile":      true,
	"WriteFile":     true,
}

// logCall writes the log line of one call. The line never carries the
// arguments of a process, its input, or its output, because they can hold
// a secret.
func (s *Server) logCall(ctx context.Context, fullMethod string, attrs []any, err error) {
	method := path.Base(fullMethod)
	level := slog.LevelDebug
	if loud[method] {
		level = slog.LevelInfo
	}
	attrs = append([]any{"method", method}, attrs...)
	if err != nil {
		level = max(level, slog.LevelInfo)
		attrs = append(attrs, "code", status.Code(err).String(), "error", status.Convert(err).Message())
	}
	s.logger().Log(ctx, level, "fw: ax guest call", attrs...)
}

// describe returns the log fields of a request: the process, the program
// without its arguments, or the file path.
func describe(req any) []any {
	switch m := req.(type) {
	case *ateenv.StartProcessRequest:
		attrs := []any{"program", program(m.GetCommand()), "stdin", m.GetStdin()}
		if m.GetCwd() != "" {
			attrs = append(attrs, "cwd", m.GetCwd())
		}
		return attrs
	case *ateenv.GetProcessRequest:
		return []any{"process", m.GetProcessId()}
	case *ateenv.StreamProcessOutputRequest:
		return []any{"process", m.GetProcessId()}
	case *ateenv.WriteProcessInputRequest:
		return []any{"process", m.GetProcessId()}
	case *ateenv.SignalProcessRequest:
		return []any{"process", m.GetProcessId(), "signal", m.GetSignal().String()}
	case *ateenv.ReadFileRequest:
		return []any{"path", m.GetPath()}
	case *ateenv.WriteFileRequest:
		return []any{"path", m.GetPath()}
	}
	return nil
}

// program is the base name of the program of a command.
func program(command []string) string {
	if len(command) == 0 {
		return ""
	}
	return path.Base(command[0])
}

// recordingStream keeps the first message that the client sends, which
// names the process or the file of a streaming call.
type recordingStream struct {
	grpc.ServerStream
	first any
}

func (r *recordingStream) RecvMsg(m any) error {
	err := r.ServerStream.RecvMsg(m)
	if err == nil && r.first == nil {
		r.first = m
	}
	return err
}
