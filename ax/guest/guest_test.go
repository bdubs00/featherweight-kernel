package guest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	ateenv "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/bdubs00/featherweight-kernel/ax"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const metaURL = "http://127.0.0.1:8080"

// syncBuffer is a log sink that a test reads while the server writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fixture is a guest server behind an h2c listener, with a client.
type fixture struct {
	srv       *Server
	url       string
	workspace string
	logs      *syncBuffer
	proc      ateenv.ProcessServiceClient
	files     ateenv.FileSystemServiceClient
}

// next answers every request that is not gRPC.
var next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "next "+r.Method+" "+r.URL.Path)
})

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{workspace: filepath.Join(t.TempDir(), "ws"), logs: &syncBuffer{}}
	if err := os.Mkdir(f.workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	task := &ax.Task{Spec: ax.TaskSpec{Debug: true}}
	srv, err := New(task, Options{
		Workspace:   f.workspace,
		MetadataURL: metaURL,
		LogDir:      t.TempDir(),
		Logger:      slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil || srv == nil {
		t.Fatalf("New = %v, %v", srv, err)
	}
	f.srv = srv
	ts := httptest.NewUnstartedServer(srv.Wrap(next))
	ts.Config.Protocols = new(http.Protocols)
	ts.Config.Protocols.SetHTTP1(true)
	ts.Config.Protocols.SetUnencryptedHTTP2(true)
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(srv.Close)
	f.url = ts.URL
	conn, err := grpc.NewClient(strings.TrimPrefix(ts.URL, "http://"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	f.proc = ateenv.NewProcessServiceClient(conn)
	f.files = ateenv.NewFileSystemServiceClient(conn)
	return f
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// exec runs a command the way ax ssh does: start, then follow the output
// to the exit.
func (f *fixture) exec(t *testing.T, ctx context.Context, req *ateenv.StartProcessRequest) (stdout, stderr string, code int32) {
	t.Helper()
	p, err := f.proc.StartProcess(ctx, req)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	return f.follow(t, ctx, p.GetProcessId())
}

func (f *fixture) follow(t *testing.T, ctx context.Context, id string) (stdout, stderr string, code int32) {
	t.Helper()
	stream, err := f.proc.StreamProcessOutput(ctx, &ateenv.StreamProcessOutputRequest{ProcessId: id, Follow: true})
	if err != nil {
		t.Fatalf("StreamProcessOutput: %v", err)
	}
	var out, errOut strings.Builder
	for {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("stream ended before the exit: %v", err)
		}
		if exit := msg.GetExit(); exit != nil {
			return out.String(), errOut.String(), exit.GetExitCode()
		}
		out.Write(msg.GetStdout())
		errOut.Write(msg.GetStderr())
	}
}

func TestNewIsOffWithoutDebug(t *testing.T) {
	for _, task := range []*ax.Task{nil, {}} {
		srv, err := New(task, Options{})
		if srv != nil || err != nil {
			t.Fatalf("New(%+v) = %v, %v", task, srv, err)
		}
		// A nil server does nothing, and Wrap returns its argument.
		srv.Close()
		rec := httptest.NewRecorder()
		srv.Wrap(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Body.String() != "next GET /healthz" {
			t.Errorf("body = %q", rec.Body.String())
		}
	}
}

func TestExecLikeAXSSH(t *testing.T) {
	ctx := testCtx(t)
	f := newFixture(t)
	out, errOut, code := f.exec(t, ctx, &ateenv.StartProcessRequest{
		Command: []string{"/bin/sh", "-c", "pwd; echo $AX_METADATA_URL; echo oops >&2; exit 3"},
	})
	if want := f.workspace + "\n" + metaURL + "\n"; out != want || errOut != "oops\n" || code != 3 {
		t.Errorf("stdout %q, stderr %q, exit %d; want stdout %q", out, errOut, code, want)
	}

	// A request that sets AX_METADATA_URL and a working directory keeps them.
	sub := filepath.Join(f.workspace, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	out, _, _ = f.exec(t, ctx, &ateenv.StartProcessRequest{
		Command: []string{"/bin/sh", "-c", "pwd; echo $AX_METADATA_URL"},
		Cwd:     sub,
		Env:     map[string]string{ax.EnvMetadataURL: "http://other"},
	})
	if want := sub + "\nhttp://other\n"; out != want {
		t.Errorf("stdout %q, want %q", out, want)
	}
}

func TestExecBeforeTheWorkspaceExists(t *testing.T) {
	ctx := testCtx(t)
	f := newFixture(t)
	if err := os.Remove(f.workspace); err != nil {
		t.Fatal(err)
	}
	if out, _, code := f.exec(t, ctx, &ateenv.StartProcessRequest{Command: []string{"pwd"}}); out != filepath.Dir(f.workspace)+"\n" || code != 0 {
		t.Errorf("stdout %q, exit %d", out, code)
	}
}

func TestStdin(t *testing.T) {
	ctx := testCtx(t)
	f := newFixture(t)
	p, err := f.proc.StartProcess(ctx, &ateenv.StartProcessRequest{Command: []string{"cat"}, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	in, err := f.proc.WriteProcessInput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Send(&ateenv.WriteProcessInputRequest{ProcessId: p.GetProcessId(), Data: []byte("hello\n"), Close: true}); err != nil {
		t.Fatal(err)
	}
	if resp, err := in.CloseAndRecv(); err != nil || resp.GetBytesWritten() != 6 {
		t.Fatalf("WriteProcessInput = %v, %v", resp, err)
	}
	if out, _, code := f.follow(t, ctx, p.GetProcessId()); out != "hello\n" || code != 0 {
		t.Errorf("stdout %q, exit %d", out, code)
	}
}

func TestFilesStayInTheWorkspace(t *testing.T) {
	ctx := testCtx(t)
	f := newFixture(t)
	w, err := f.files.WriteFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Send(&ateenv.WriteFileRequest{Path: "notes.txt", Chunk: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(f.workspace, "notes.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("file = %q, %v", b, err)
	}

	read := func(path string) (string, error) {
		r, err := f.files.ReadFile(ctx, &ateenv.ReadFileRequest{Path: path})
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for {
			msg, err := r.Recv()
			if errors.Is(err, io.EOF) {
				return sb.String(), nil
			}
			if err != nil {
				return "", err
			}
			sb.Write(msg.GetChunk())
		}
	}
	if got, err := read("notes.txt"); err != nil || got != "hi" {
		t.Errorf("ReadFile = %q, %v", got, err)
	}
	outside := filepath.Join(filepath.Dir(f.workspace), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, "../outside.txt"} {
		if got, err := read(path); err == nil || status.Code(err) == codes.NotFound {
			t.Errorf("ReadFile(%s) = %q, %v; want a refusal", path, got, err)
		}
	}
}

func TestWrapSendsOnlyGRPCToTheServices(t *testing.T) {
	f := newFixture(t)
	for _, req := range []*http.Request{
		mustRequest(t, http.MethodGet, f.url+"/healthz", ""),
		// HTTP/1.1 never reaches the services, whatever its content type.
		mustRequest(t, http.MethodPost, f.url+"/ateenv.v1alpha.ProcessService/StartProcess", "application/grpc"),
	} {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.HasPrefix(string(b), "next ") {
			t.Errorf("%s %s = %q", req.Method, req.URL.Path, b)
		}
	}
}

func mustRequest(t *testing.T, method, url, contentType string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func TestLogLinesNameTheProgramOnly(t *testing.T) {
	ctx := testCtx(t)
	f := newFixture(t)
	if out, _, _ := f.exec(t, ctx, &ateenv.StartProcessRequest{Command: []string{"/bin/sh", "-c", "echo s3cr3t-value"}}); out != "s3cr3t-value\n" {
		t.Fatalf("stdout %q", out)
	}
	if _, err := f.proc.StartProcess(ctx, &ateenv.StartProcessRequest{Command: []string{"/no/such/program"}}); err == nil {
		t.Fatal("a missing program started")
	}
	logs := f.logs.String()
	for _, want := range []string{
		`"msg":"fw: ax guest call","method":"StartProcess","program":"sh"`,
		`"process":"proc-`,
		`"method":"StreamProcessOutput"`,
		`"program":"program"`,
		`"code":"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "s3cr3t") {
		t.Errorf("logs carry the arguments or the output:\n%s", logs)
	}
}

func TestCloseEndsAFollowingCall(t *testing.T) {
	ctx := testCtx(t)
	f := newFixture(t)
	p, err := f.proc.StartProcess(ctx, &ateenv.StartProcessRequest{Command: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(int(p.GetPid()), syscall.SIGKILL) })
	stream, err := f.proc.StreamProcessOutput(ctx, &ateenv.StreamProcessOutputRequest{ProcessId: p.GetProcessId(), Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				done <- err
				return
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	f.srv.Close()
	select {
	case err := <-done:
		if status.Code(err) == codes.OK {
			t.Errorf("stream ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the call")
	}
	f.srv.Close() // a second Close does nothing
}
