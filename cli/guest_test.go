package cli

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ateenv "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/bdubs00/featherweight-kernel/fw"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// gateModel holds each model call until gate closes. started closes when
// the first call arrives.
type gateModel struct {
	scriptModel
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func (g *gateModel) Generate(ctx context.Context, req *fw.Request) (*fw.Response, error) {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.scriptModel.Generate(ctx, req)
}

func (g *gateModel) Stream(ctx context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
	return g.Generate(ctx, req)
}

// guestExec runs a shell script through the guest services at addr, as
// ax ssh does, and returns its standard output.
func guestExec(t *testing.T, addr, script string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	proc := ateenv.NewProcessServiceClient(conn)
	p, err := proc.StartProcess(ctx, &ateenv.StartProcessRequest{Command: []string{"/bin/sh", "-c", script}})
	if err != nil {
		return "", err
	}
	stream, err := proc.StreamProcessOutput(ctx, &ateenv.StreamProcessOutputRequest{ProcessId: p.GetProcessId(), Follow: true})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for {
		msg, err := stream.Recv()
		if err != nil {
			return out.String(), err
		}
		if msg.GetExit() != nil {
			return out.String(), nil
		}
		out.Write(msg.GetStdout())
	}
}

func TestServeAXGuestServices(t *testing.T) {
	m := &gateModel{
		scriptModel: scriptModel{resps: []*fw.Response{endTurn("bootstrapped")}},
		gate:        make(chan struct{}),
		started:     make(chan struct{}),
	}
	env(t, fw.WithModel(m))
	dir := t.TempDir()
	taskFile, wsFile, wsPath := writeManifests(t, dir, "  debug: true")
	addr := freeAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	stop := serveInBackground(t, "serve", "--task-file", taskFile, "--workspace-file", wsFile, "--listen", addr, "--home", filepath.Join(wsPath, ".fw"))
	want := wsPath + "\nhttp://127.0.0.1:" + port + "\n"

	// The workspace goal waits on the model, so the boot is not done, and
	// ax ssh works already.
	select {
	case <-m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the workspace goal did not start")
	}
	if code, _ := get(t, "http://"+addr+"/readyz"); code != 503 {
		t.Errorf("readyz during the boot = %d", code)
	}
	if out, err := guestExec(t, addr, "pwd; echo $AX_METADATA_URL"); err != nil || out != want {
		t.Errorf("during the boot: stdout %q, %v; want %q", out, err, want)
	}

	close(m.gate)
	waitFor(t, "http://"+addr+"/readyz", 200)
	if out, err := guestExec(t, addr, "pwd; echo $AX_METADATA_URL"); err != nil || out != want {
		t.Errorf("after the boot: stdout %q, %v; want %q", out, err, want)
	}
	// The other surfaces still answer on the same port.
	if code, _ := get(t, "http://"+addr+"/healthz"); code != 200 {
		t.Errorf("healthz = %d", code)
	}

	code, _, errOut := stop()
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, s := range []string{"guest services of ax ssh are on", "fw: ax guest call", "StartProcess"} {
		if !strings.Contains(errOut, s) {
			t.Errorf("stderr lacks %q:\n%s", s, errOut)
		}
	}
}

func TestServeAXGuestServicesNeedDebug(t *testing.T) {
	env(t, fw.WithModel(&scriptModel{resps: []*fw.Response{endTurn("bootstrapped")}}))
	dir := t.TempDir()
	taskFile, wsFile, wsPath := writeManifests(t, dir, "")
	addr := freeAddr(t)
	stop := serveInBackground(t, "serve", "--task-file", taskFile, "--workspace-file", wsFile, "--listen", addr, "--home", filepath.Join(wsPath, ".fw"))
	waitFor(t, "http://"+addr+"/readyz", 200)
	if out, err := guestExec(t, addr, "echo ran"); err == nil {
		t.Errorf("a Task without spec.debug ran a process: %q", out)
	}
	if code, _, errOut := stop(); code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
}
