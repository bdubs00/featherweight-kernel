package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// ocBlock returns the block harness of a test of opencode: a fake command
// that records its arguments and its environment and then sleeps, and the
// port of the fake server.
func ocBlock(d testDirs, f *fakeOC) string {
	script := fmt.Sprintf(`printf '%%s\n' "$@" > %[1]s/oc-args; env > %[1]s/oc-env.tmp; mv %[1]s/oc-env.tmp %[1]s/oc-env; exec sleep 600`, d.root)
	cmd, _ := json.Marshal([]string{"sh", "-c", script, "fake-opencode"})
	return fmt.Sprintf("kind: opencode\nport: %d\ncommand: %s\n", f.port(), cmd)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOpencodeRun(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, writeHello)
	cfg := testConfig(t, d, ocBlock(d, f))
	cfg.Budget.MaxTurns = 10
	l := fw.NewMemLedger()
	a := newAgent(t, cfg, l)
	r, err := a.Start(testCtx(t), "Create hello.txt with the text hello", fw.WithInstructions("Be brief."))
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateCompleted || r.Output != "Done." {
		t.Fatalf("run = %+v", r)
	}
	if r.Usage.Turns != 2 || r.Usage.InputTokens != 230 || r.Usage.OutputTokens != 25 || r.Usage.ToolCalls != 1 {
		t.Fatalf("usage = %+v", r.Usage)
	}
	evs := events(t, l, r.ID)
	noSecret(t, evs)
	// The first model call asked about its tool call, so its response came
	// before the call ran, and its usage after it.
	resp := ofType(evs, fw.EvModelResponded)
	var first fw.ModelRespondedData
	resp[0].Decode(&first)
	if len(resp) != 2 || first.StopReason != fw.StopToolUse || len(first.Content) != 2 || first.Content[1].Name != "write" || first.Provider != "openai" || first.Model != "test-model" {
		t.Fatalf("responses = %d, first = %+v", len(resp), first)
	}
	var req fw.ToolRequestedData
	ofType(evs, fw.EvToolRequested)[0].Decode(&req)
	var res fw.ToolRespondedData
	ofType(evs, fw.EvToolResponded)[0].Decode(&res)
	if req.Tool != "write" || req.CallID != res.CallID || res.IsError || fw.TextOf(res.Content) != "Wrote hello.txt" {
		t.Fatalf("tool call = %+v, result = %+v", req, res)
	}
	var fc fw.FileChangedData
	if ch := ofType(evs, fw.EvFileChanged); len(ch) != 1 || ch[0].Decode(&fc) != nil || fc.Path != "hello.txt" || fc.Op != fw.FileCreated || fc.Size != 5 || fc.Tool != KindOpencode {
		t.Fatalf("file events = %v, %+v", ch, fc)
	}
	if files := fw.RunFiles(evs); len(files) != 1 || files[0].Path != "hello.txt" {
		t.Fatalf("run files = %+v", files)
	}
	kinds := strings.Join(harnessKinds(evs), " ")
	for _, want := range []string{KindStart, KindSession, KindPrompt, "permission.asked", "message.part.updated", KindIdle} {
		if !strings.Contains(kinds, want) {
			t.Fatalf("the harness events %q lack %s", kinds, want)
		}
	}
	// The kernel answered the permission from its policy, which allows a
	// write by default.
	if len(f.replies) != 1 || f.replies[0].Reply != "once" || f.replies[0].Route != "permission" {
		t.Fatalf("replies = %+v", f.replies)
	}
	if len(f.prompts) != 1 || f.prompts[0].Text != "Create hello.txt with the text hello" || f.prompts[0].System != "Be brief." ||
		f.prompts[0].Model["providerID"] != "fw-openai" || f.prompts[0].Model["modelID"] != "test-model" || f.prompts[0].Dir != d.ws {
		t.Fatalf("prompts = %+v", f.prompts)
	}
	if f.noAuth {
		t.Fatal("a request to opencode came without basic authentication")
	}
	// The process got the home, the key, and a configuration that names
	// the variable of the key, never the key.
	// The fake server answers before the command wrote its files.
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(d.root, "oc-env")); return err == nil })
	env := readFile(t, filepath.Join(d.root, "oc-env"))
	for _, want := range []string{"HOME=" + d.home + "\n", "FW_TEST_MODEL_KEY=" + testKey + "\n", "OPENCODE_DISABLE_MODELS_FETCH=1", `"baseURL":"http://127.0.0.1:9/v1"`, `"apiKey":"{env:FW_TEST_MODEL_KEY}"`, `OPENCODE_PERMISSION={"*":"ask"`} {
		if !strings.Contains(env, want) {
			t.Fatalf("the environment of opencode lacks %q", want)
		}
	}
	var conf string
	for _, line := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(line, "OPENCODE_CONFIG_CONTENT="); ok {
			conf = v
		}
	}
	if strings.Contains(conf, testKey) || !json.Valid([]byte(conf)) {
		t.Fatalf("the configuration of opencode = %s", conf)
	}
	if args := readFile(t, filepath.Join(d.root, "oc-args")); args != fmt.Sprintf("--port\n%d\n--hostname\n127.0.0.1\n", f.port()) {
		t.Fatalf("args = %q", args)
	}
	if st, err := os.Stat(d.home); err != nil || !st.IsDir() {
		t.Fatalf("home: %v", err)
	}

	// A follow-up goes to the same session.
	f.script = answerOnly
	if err := a.Send(testCtx(t), r.ID, fw.Message{Content: []fw.Block{fw.Text("Say it again")}}, fw.OriginCLI); err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, func(r *fw.Run) bool { return terminal(r) && r.Output == "Again." })
	if len(f.prompts) != 2 || f.prompts[1].Session != f.prompts[0].Session || f.prompts[1].Text != "Say it again" {
		t.Fatalf("prompts = %+v", f.prompts)
	}
}

func TestOpencodeDeny(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, writeHello)
	f.oldRoute = true
	cfg := testConfig(t, d, ocBlock(d, f))
	cfg.Policy.Rules = fw.Rules{Default: "allow", Deny: []string{"write: filePath=hello.txt"}}
	l := fw.NewMemLedger()
	a := newAgent(t, cfg, l)
	r, err := a.Start(testCtx(t), "Create hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateCompleted {
		t.Fatalf("run = %+v", r)
	}
	// The older route takes the answer, without the reason.
	if len(f.replies) != 1 || f.replies[0].Reply != "reject" || f.replies[0].Route != "session" {
		t.Fatalf("replies = %+v", f.replies)
	}
	var res fw.ToolRespondedData
	ofType(events(t, l, r.ID), fw.EvToolResponded)[0].Decode(&res)
	if !res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(d.ws, "hello.txt")); err == nil {
		t.Fatal("a denied call wrote the file")
	}
}

// TestOpencodeApprovalAfterColdBoot asks a person about a call, stops the
// agent and the server while the run waits, as a suspend of the Task
// does, and decides in a new agent. The new agent starts opencode again,
// tells the session the decision, and answers the call that the session
// makes again from that decision.
func TestOpencodeApprovalAfterColdBoot(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, writeHello)
	block := ocBlock(d, f)
	cfg := testConfig(t, d, block)
	cfg.Policy.Rules = fw.Rules{Default: "allow", Ask: []string{"write"}}
	l := fw.NewMemLedger()
	a := newAgent(t, cfg, l)
	r, err := a.Start(testCtx(t), "Create hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, inputRequired)
	if r.Pending == nil || r.Pending.Tool != "write" || !strings.Contains(string(r.Pending.Input), "hello.txt") {
		t.Fatalf("pending = %+v", r.Pending)
	}
	pendingCall := r.Pending.CallID
	closeAgent(a)
	f.restart()

	cfg = testConfig(t, d, block)
	cfg.Policy.Rules = fw.Rules{Default: "allow", Ask: []string{"write"}}
	b := newAgent(t, cfg, l)
	if got := waitState(t, b, r.ID, inputRequired); got.Pending == nil || got.Pending.ID != r.Pending.ID {
		t.Fatalf("after the restart: %+v", got)
	}
	if err := b.Decide(testCtx(t), r.ID, fw.ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow", Decider: "ana"}); err != nil {
		t.Fatal(err)
	}
	r = waitState(t, b, r.ID, terminal)
	if r.State != fw.StateCompleted || r.Output != "Done." {
		t.Fatalf("run = %+v", r)
	}
	if len(f.prompts) != 2 || !strings.Contains(f.prompts[1].Text, "stopped and started again") ||
		!strings.Contains(f.prompts[1].Text, "A person allowed the call write") || f.prompts[1].Session != f.prompts[0].Session {
		t.Fatalf("prompts = %+v", f.prompts)
	}
	// The session made the call again; the decision answered it, and no
	// second approval came.
	if len(f.replies) != 1 || f.replies[0].Reply != "once" {
		t.Fatalf("replies = %+v", f.replies)
	}
	evs := events(t, l, r.ID)
	if n := len(ofType(evs, fw.EvApprovalRequested)); n != 1 {
		t.Fatalf("%d approvals", n)
	}
	var first fw.ToolRespondedData
	for _, e := range ofType(evs, fw.EvToolResponded) {
		var d fw.ToolRespondedData
		e.Decode(&d)
		if d.CallID == pendingCall {
			first = d
		}
	}
	if !first.IsError || !strings.Contains(fw.TextOf(first.Content), "not finished") {
		t.Fatalf("the interrupted call = %+v", first)
	}
	if got := readFile(t, filepath.Join(d.ws, "hello.txt")); got != "hello" {
		t.Fatalf("hello.txt = %q", got)
	}
	noSecret(t, evs)
}

// TestOpencodeApprovalLive decides while the session waits in the same
// process: the kernel answers the permission that waits.
func TestOpencodeApprovalLive(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, writeHello)
	cfg := testConfig(t, d, ocBlock(d, f))
	cfg.Policy.Rules = fw.Rules{Default: "allow", Ask: []string{"write"}}
	l := fw.NewMemLedger()
	a := newAgent(t, cfg, l)
	r, err := a.Start(testCtx(t), "Create hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, inputRequired)
	if err := a.Decide(testCtx(t), r.ID, fw.ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "deny", Note: "not now"}); err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateCompleted || len(f.prompts) != 1 {
		t.Fatalf("run = %+v, prompts = %d", r, len(f.prompts))
	}
	if len(f.replies) != 1 || f.replies[0].Reply != "reject" || !strings.Contains(f.replies[0].Message, "not now") {
		t.Fatalf("replies = %+v", f.replies)
	}
}

func TestOpencodeBudget(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, writeHello)
	cfg := testConfig(t, d, ocBlock(d, f))
	cfg.Budget.MaxTurns = 1
	l := fw.NewMemLedger()
	a := newAgent(t, cfg, l)
	r, err := a.Start(testCtx(t), "Create hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateFailed || !strings.Contains(r.Error, "maxTurns") || r.Usage.Turns != 1 {
		t.Fatalf("run = %+v", r)
	}
	var ex fw.BudgetExceededData
	if e := ofType(events(t, l, r.ID), fw.EvBudgetExceeded); len(e) != 1 || e[0].Decode(&ex) != nil || ex.Limit != "maxTurns" {
		t.Fatalf("budget events = %v", e)
	}
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.aborts) == 1
	})
	// The file that the first turn wrote is in the ledger.
	if ch := ofType(events(t, l, r.ID), fw.EvFileChanged); len(ch) != 1 {
		t.Fatalf("file events = %d", len(ch))
	}
}

// TestOpencodeCancelWhileWaiting cancels a run whose session waits for a
// person: the kernel aborts the session.
func TestOpencodeCancelWhileWaiting(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, writeHello)
	cfg := testConfig(t, d, ocBlock(d, f))
	cfg.Policy.Rules = fw.Rules{Default: "allow", Ask: []string{"write"}}
	a := newAgent(t, cfg, fw.NewMemLedger())
	r, err := a.Start(testCtx(t), "Create hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a, r.ID, inputRequired)
	if err := a.Cancel(testCtx(t), r.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.aborts) == 1 && f.aborts[0] == f.prompts[0].Session
	})
}

// TestHarnessMissing fails the run with a message that names the program
// when the image has no harness.
func TestHarnessMissing(t *testing.T) {
	for _, kind := range []string{KindOpencode, KindClaudeCode} {
		d := newDirs(t)
		cfg := testConfig(t, d, "kind: "+kind+"\ncommand: [/nonexistent/harness]\n")
		if kind == KindClaudeCode {
			cfg.Model = "anthropic/claude-test"
		}
		a := newAgent(t, cfg, fw.NewMemLedger())
		if kind == KindClaudeCode {
			// The hook route needs the native API.
			a.Runner().(*host).mounts = 1
		}
		r, err := a.Start(testCtx(t), "hello")
		if err != nil {
			t.Fatal(err)
		}
		r = waitState(t, a, r.ID, terminal)
		if r.State != fw.StateFailed || !strings.Contains(r.Error, "/nonexistent/harness") {
			t.Fatalf("%s: run = %+v", kind, r)
		}
	}
}

// TestFailedRunEndsOnce fails a run before the harness got its input: the
// run stays finished, and the driver does not start it again.
func TestFailedRunEndsOnce(t *testing.T) {
	for _, kind := range []string{KindOpencode, KindClaudeCode} {
		d := newDirs(t)
		cfg := testConfig(t, d, "kind: "+kind+"\ncommand: [/nonexistent/harness]\n")
		if kind == KindClaudeCode {
			cfg.Model = "anthropic/claude-test"
		}
		l := fw.NewMemLedger()
		a := newAgent(t, cfg, l)
		if kind == KindClaudeCode {
			a.Runner().(*host).mounts = 1
		}
		r, err := a.Start(testCtx(t), "hello")
		if err != nil {
			t.Fatal(err)
		}
		waitState(t, a, r.ID, terminal)
		time.Sleep(300 * time.Millisecond)
		evs := events(t, l, r.ID)
		if f, e := len(ofType(evs, fw.EvRunFinished)), len(ofType(evs, fw.EvError)); f != 1 || e != 1 {
			t.Fatalf("%s: %d events, %d fw.run.finished, %d fw.error", kind, len(evs), f, e)
		}
		closeAgent(a)
	}
}

// TestOpencodeFollowUpWithoutText sends a blank follow-up and one with an
// attachment alone. The blank one gives the session nothing, and the run
// finishes again at once; the other one names the file.
func TestOpencodeFollowUpWithoutText(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, writeHello)
	cfg := testConfig(t, d, ocBlock(d, f))
	l := fw.NewMemLedger()
	a := newAgent(t, cfg, l)
	r, err := a.Start(testCtx(t), "Create hello.txt with the text hello")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a, r.ID, terminal)
	if err := a.Send(testCtx(t), r.ID, fw.Message{Role: fw.RoleUser, Content: []fw.Block{fw.Text("   ")}}, fw.OriginCLI); err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateCompleted || r.Output != "Done." {
		t.Fatalf("after a blank follow-up: %+v", r)
	}
	time.Sleep(200 * time.Millisecond)
	n := len(events(t, l, r.ID))
	time.Sleep(200 * time.Millisecond)
	if m := len(events(t, l, r.ID)); m != n {
		t.Fatalf("the run recorded %d events after its end", m-n)
	}
	f.mu.Lock()
	prompts := len(f.prompts)
	f.mu.Unlock()
	if prompts != 1 {
		t.Fatalf("the session got %d prompts", prompts)
	}

	f.script = answerOnly
	if err := a.Send(testCtx(t), r.ID, fw.Message{Content: []fw.Block{fw.Attachment("notes.txt", []byte("some notes"))}}, fw.OriginCLI); err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, func(r *fw.Run) bool { return terminal(r) && r.Output == "Again." })
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) != 2 || f.prompts[1].Text != "The person sent files without text: attachments/notes.txt." {
		t.Fatalf("prompts = %+v", f.prompts)
	}
}

// TestOpencodeBranchFails branches a run whose session started: the
// branch fails, and the session of the source gets no prompt from it.
func TestOpencodeBranchFails(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, writeHello)
	cfg := testConfig(t, d, ocBlock(d, f))
	l := fw.NewMemLedger()
	a := newAgent(t, cfg, l)
	r, err := a.Start(testCtx(t), "Create hello.txt with the text hello")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a, r.ID, terminal)
	b, err := a.Fork(testCtx(t), r.ID, fw.ForkRequest{Input: "Do it again"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	br := waitState(t, a, b.ID, terminal)
	if br.State != fw.StateFailed || br.Error != errBranch {
		t.Fatalf("branch = %+v", br)
	}
	if n := len(ofType(events(t, l, b.ID), fw.EvError)); n != 1 {
		t.Fatalf("%d fw.error events", n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) != 1 {
		t.Fatalf("prompts = %+v", f.prompts)
	}
}

// TestOpencodeSilence follows a session that sends nothing: the driver
// counts the stream as lost after the quiet time, starts again twice, and
// fails the run.
func TestOpencodeSilence(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, silent)
	cfg := testConfig(t, d, ocBlock(d, f))
	a := newAgent(t, cfg, fw.NewMemLedger())
	h := a.Runner().(*host)
	h.tick, h.quiet = 20*time.Millisecond, 200*time.Millisecond
	r, err := a.Start(testCtx(t), "hello")
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateFailed || !strings.Contains(r.Error, "sent no event") {
		t.Fatalf("run = %+v", r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) != 3 || len(f.aborts) != 3 {
		t.Fatalf("prompts = %d, aborts = %d", len(f.prompts), len(f.aborts))
	}
}

// TestOpencodeDeadlineWithoutEvents follows a session that sends nothing
// until the deadline of the run passes: the tick stops the run.
func TestOpencodeDeadlineWithoutEvents(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, silent)
	cfg := testConfig(t, d, ocBlock(d, f))
	l := fw.NewMemLedger()
	a := newAgent(t, cfg, l)
	a.Runner().(*host).tick = 20 * time.Millisecond
	r, err := a.Start(testCtx(t), "hello", fw.WithRunBudget(fw.Budget{Deadline: 300 * time.Millisecond}))
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateFailed || !strings.Contains(r.Error, "deadline") {
		t.Fatalf("run = %+v", r)
	}
	if n := len(ofType(events(t, l, r.ID), fw.EvBudgetExceeded)); n != 1 {
		t.Fatalf("%d budget events", n)
	}
}

// TestOpencodeReadOfTheHome reads a file of the home of the harness with
// the read tool, which the default policy allows: the kernel rejects it.
func TestOpencodeReadOfTheHome(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, readAt("../.harness/.local/share/opencode/storage/session/info.json"))
	cfg := testConfig(t, d, ocBlock(d, f))
	a := newAgent(t, cfg, fw.NewMemLedger())
	r, err := a.Start(testCtx(t), "Read the sessions")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a, r.ID, terminal)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.replies) != 1 || f.replies[0].Reply != "reject" || !strings.Contains(f.replies[0].Message, "inside the home of the harness") {
		t.Fatalf("replies = %+v", f.replies)
	}
}
