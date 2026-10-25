//go:build linux

package fw

import (
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// rootWorkspace returns a workspace and a home under it for a test that
// runs as root, with the directories above it open to other users, as
// /workspace is in an ax Task. It skips the test without root.
func rootWorkspace(t *testing.T) (ws, home string) {
	t.Helper()
	if os.Geteuid() != 0 || len(MissingToolCaps()) > 0 {
		t.Skip("needs root with the capabilities of tools.user to start a process as another user")
	}
	ws = evalExisting(t.TempDir())
	for d := ws; d != filepath.Dir(d); d = filepath.Dir(d) {
		if info, err := os.Stat(d); err == nil && info.Mode().Perm()&0o005 != 0o005 {
			if err := os.Chmod(d, info.Mode().Perm()|0o005); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ws, filepath.Join(ws, ".fw")
}

// Root without a capability of tools.user, as in gVisor under Agent
// Substrate, refuses a tool user with an error that names the capability.
func TestToolUserNeedsCapabilities(t *testing.T) {
	missing := MissingToolCaps()
	if os.Geteuid() != 0 || len(missing) == 0 {
		t.Skip("needs root without a capability of tools.user")
	}
	err := canBecome(&ToolUser{UID: freeID, GID: freeID})
	if err == nil || !strings.Contains(err.Error(), missing[0]) || !strings.Contains(err.Error(), "set tools.user: none") {
		t.Fatalf("canBecome = %v", err)
	}
}

func ownerOf(t *testing.T, p string) (uid, gid uint32, mode fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	return st.Uid, st.Gid, info.Mode()
}

// asTool runs a shell command as u and returns its output and error.
func asTool(u *ToolUser, dir, script string) (string, error) {
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	u.Apply(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// With a tool user, FW_HOME belongs to fw with mode 0700, the rest of the
// workspace belongs to the tool user, and the directory that holds FW_HOME
// is sticky, so a tool process reads and changes the workspace but never
// reads, changes, or replaces the state of fw.
func TestToolUserOwnsTheWorkspace(t *testing.T) {
	ws, home := rootWorkspace(t)
	mustWrite(t, filepath.Join(ws, "app", "main.go"), "package main\n")
	mustWrite(t, filepath.Join(home, "fw.yaml"), "model: none\n")
	mustWrite(t, filepath.Join(home, "runs", "r1", "ledger.jsonl"), "{}\n")
	cfg := DefaultConfig()
	cfg.Home = home
	cfg.ConfigFile = filepath.Join(home, "fw.yaml")
	cfg.Tools.Files.Root = ws
	cfg.Tools.User = strconv.Itoa(freeID) + ":" + strconv.Itoa(freeID)
	a := newTestAgent(t, newScript().Model(), WithConfig(cfg))
	u := a.ToolUser()
	if u == nil || u.UID != freeID {
		t.Fatalf("ToolUser = %v", u)
	}
	if uid, _, mode := ownerOf(t, home); uid != 0 || mode.Perm() != 0o700 {
		t.Errorf("FW_HOME: uid %d, mode %v", uid, mode)
	}
	if uid, gid, mode := ownerOf(t, ws); uid != 0 || gid != freeID || mode&os.ModeSticky == 0 || mode.Perm() != 0o775 {
		t.Errorf("the directory of FW_HOME: uid %d gid %d mode %v", uid, gid, mode)
	}
	for _, p := range []string{"app", "app/main.go"} {
		if uid, gid, _ := ownerOf(t, filepath.Join(ws, p)); uid != freeID || gid != freeID {
			t.Errorf("%s: %d:%d", p, uid, gid)
		}
	}
	if uid, _, _ := ownerOf(t, filepath.Join(home, "runs", "r1", "ledger.jsonl")); uid != 0 {
		t.Errorf("the walk changed a file of FW_HOME")
	}
	if _, err := os.Stat(ToolTreeMarker(home)); err != nil {
		t.Errorf("no marker: %v", err)
	}

	// A file that the kernel writes for a tool goes to the tool user.
	if err := a.Files().WriteFile("attachments/new/chart.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"attachments", "attachments/new", "attachments/new/chart.txt"} {
		if uid, _, _ := ownerOf(t, filepath.Join(ws, p)); uid != freeID {
			t.Errorf("%s belongs to %d", p, uid)
		}
	}

	if out, err := asTool(u, ws, "echo changed >> app/main.go && mkdir -p out && echo ok > out/x && cat app/main.go"); err != nil {
		t.Fatalf("the tool user cannot work in the workspace: %v: %s", err, out)
	}
	for _, script := range []string{
		"cat .fw/fw.yaml",
		"ls .fw/runs",
		"echo x > .fw/fw.yaml",
		"mv .fw stolen",
		"rm -rf .fw",
		"cat /proc/" + strconv.Itoa(os.Getpid()) + "/environ",
	} {
		if out, err := asTool(u, ws, script); err == nil {
			t.Errorf("the tool user ran %q: %s", script, out)
		}
	}
	if b, err := os.ReadFile(filepath.Join(home, "fw.yaml")); err != nil || string(b) != "model: none\n" {
		t.Errorf("fw.yaml = %q, %v", b, err)
	}
}

// A token file that the tool user can read stops the start.
func TestToolUserRefusesAReadableTokenFile(t *testing.T) {
	ws, home := rootWorkspace(t)
	token := filepath.Join(ws, "token")
	mustWrite(t, token, "fleet-token-123456\n")
	if err := os.Chmod(token, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FW_API_TOKEN_FILE", token)
	cfg := DefaultConfig()
	cfg.Home = home
	cfg.Tools.Files.Root = filepath.Join(ws, "app")
	cfg.Tools.User = strconv.Itoa(freeID) + ":" + strconv.Itoa(freeID)
	_, err := New(WithConfig(cfg), WithoutDefaults(), WithAuthn(NoAuthn{}), WithModel(newScript().Model()), WithLedger(NewMemLedger()), WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "can read the token file") {
		t.Fatalf("New = %v", err)
	}
	if err := os.Chmod(token, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(WithConfig(cfg), WithoutDefaults(), WithAuthn(NoAuthn{}), WithModel(newScript().Model()), WithLedger(NewMemLedger()), WithLogger(quietLogger()))
	if err != nil {
		t.Fatalf("New with a private token file = %v", err)
	}
	a.Close(testCtx(t))
}

// The root of the files tool stays with fw in a sticky directory, so a
// tool process cannot rename it and put a link to FW_HOME in its place.
func TestToolUserCannotReplaceTheFilesRoot(t *testing.T) {
	ws, home := rootWorkspace(t)
	mustWrite(t, filepath.Join(ws, "app", "main.go"), "package main\n")
	cfg := DefaultConfig()
	cfg.Home = home
	cfg.Tools.Files.Root = filepath.Join(ws, "app")
	cfg.Workspaces = []string{ws}
	cfg.Tools.User = strconv.Itoa(freeID) + ":" + strconv.Itoa(freeID)
	a := newTestAgent(t, newScript().Model(), WithConfig(cfg))
	u := a.ToolUser()
	for _, d := range []string{ws, filepath.Join(ws, "app")} {
		if uid, gid, mode := ownerOf(t, d); uid != 0 || gid != freeID || mode&os.ModeSticky == 0 {
			t.Errorf("%s: uid %d gid %d mode %v", d, uid, gid, mode)
		}
	}
	if uid, _, _ := ownerOf(t, filepath.Join(ws, "app", "main.go")); uid != freeID {
		t.Errorf("main.go belongs to %d", uid)
	}
	for _, script := range []string{"mv app app2", "rm -rf app"} {
		if out, err := asTool(u, ws, script); err == nil {
			t.Errorf("the tool user ran %q: %s", script, out)
		}
	}
	if uid, _, mode := ownerOf(t, filepath.Join(ws, "app")); uid != 0 || !mode.IsDir() {
		t.Errorf("the root of the files tool changed: uid %d, mode %v", uid, mode)
	}
	if out, err := asTool(u, filepath.Join(ws, "app"), "echo // more >> main.go && mkdir pkg && touch pkg/a.go"); err != nil {
		t.Errorf("the tool user cannot work in the root: %v: %s", err, out)
	}
}

// With tools.files.perRun, the directory of the runs and the directory of
// each run belong to fw, so the tool user works in the directory of a run
// but cannot rename it or put a link in its place, and a directory of a
// run that the tool user made is refused.
func TestToolUserAndRunDirectories(t *testing.T) {
	ws, home := rootWorkspace(t)
	cfg := DefaultConfig()
	cfg.Home = home
	cfg.Tools.Files.Root = filepath.Join(ws, "app")
	cfg.Workspaces = []string{ws}
	on := true
	cfg.Tools.Files.PerRun = &on
	cfg.Tools.User = strconv.Itoa(freeID) + ":" + strconv.Itoa(freeID)
	a := newTestAgent(t, newScript().Model(), WithConfig(cfg))
	u := a.ToolUser()
	runs := filepath.Join(ws, "app", RunsDir)
	if uid, gid, mode := ownerOf(t, runs); uid != 0 || gid != freeID || mode&os.ModeSticky == 0 {
		t.Fatalf("runs: uid %d gid %d mode %v", uid, gid, mode)
	}
	w, err := a.runWorkspace(path.Join(RunsDir, "r1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if uid, gid, mode := ownerOf(t, filepath.Join(runs, "r1")); uid != 0 || gid != freeID || mode.Perm() != 0o775 || mode&os.ModeSticky != 0 {
		t.Fatalf("the directory of a run: uid %d gid %d mode %v", uid, gid, mode)
	}
	if err := w.WriteFile("made/by/fw.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if uid, _, _ := ownerOf(t, filepath.Join(runs, "r1", "made", "by", "fw.txt")); uid != freeID {
		t.Errorf("a file of the files tool belongs to %d", uid)
	}
	if out, err := asTool(u, filepath.Join(runs, "r1"), "mkdir src && echo x > src/a && rm -rf made"); err != nil {
		t.Fatalf("the tool user cannot work in the directory of its run: %v: %s", err, out)
	}
	for _, script := range []string{"mv r1 r9", "rm -rf r1", "mkdir -p empty/r1 && mv -T empty/r1 r1"} {
		if out, err := asTool(u, runs, script); err == nil {
			t.Errorf("the tool user ran %q: %s", script, out)
		}
	}
	if uid, _, mode := ownerOf(t, filepath.Join(runs, "r1")); uid != 0 || !mode.IsDir() {
		t.Errorf("the directory of the run changed: uid %d, mode %v", uid, mode)
	}
	if out, err := asTool(u, runs, "mkdir r3"); err != nil {
		t.Fatalf("mkdir: %v: %s", err, out)
	}
	if _, err := a.runWorkspace(path.Join(RunsDir, "r3"), nil); err == nil || !strings.Contains(err.Error(), "belongs to the tool user") {
		t.Fatalf("a directory that the tool user made = %v", err)
	}
}

// tools.userHome gives the tool user a home of its own with the mode
// 0700, where a tool process writes. A link that the tool user put in its
// place in an earlier start goes first, and a home that holds FW_HOME
// stops the start.
func TestToolUserHome(t *testing.T) {
	ws, home := rootWorkspace(t)
	toolHome := filepath.Join(ws, ".home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// A link to FW_HOME in place of the home, as a tool process can leave.
	if err := os.Symlink(home, toolHome); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Home = home
	cfg.Tools.Files.Root = ws
	cfg.Tools.User = strconv.Itoa(freeID) + ":" + strconv.Itoa(freeID)
	cfg.Tools.UserHome = toolHome
	a := newTestAgent(t, newScript().Model(), WithConfig(cfg))
	u := a.ToolUser()
	if u == nil || u.Home != toolHome {
		t.Fatalf("ToolUser = %+v", u)
	}
	if uid, gid, mode := ownerOf(t, toolHome); uid != freeID || gid != freeID || !mode.IsDir() || mode.Perm() != 0o700 {
		t.Fatalf("the home: %d:%d %v", uid, gid, mode)
	}
	if uid, _, mode := ownerOf(t, home); uid != 0 || mode.Perm() != 0o700 {
		t.Fatalf("FW_HOME: uid %d, mode %v", uid, mode)
	}
	if out, err := asTool(u, ws, `echo "$HOME" && touch "$HOME/.bash_history" && mkdir -p "$HOME/.cache/x"`); err != nil || strings.TrimSpace(out) != toolHome {
		t.Fatalf("the tool user cannot use its home: %v: %s", err, out)
	}
	// The next start finds the home of the tool user and keeps it. A home
	// with another mode gets 0700 again, also without CAP_FOWNER.
	newTestAgent(t, newScript().Model(), WithConfig(cfg))
	if uid, _, mode := ownerOf(t, toolHome); uid != freeID || mode.Perm() != 0o700 {
		t.Fatalf("the home at the next start: %d %v", uid, mode)
	}
	if _, err := os.Stat(filepath.Join(toolHome, ".bash_history")); err != nil {
		t.Fatalf("the next start lost a file of the home: %v", err)
	}
	// Root without CAP_FOWNER changes the mode of its own files only.
	if err := os.Lchown(toolHome, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(toolHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(toolHome, freeID, freeID); err != nil {
		t.Fatal(err)
	}
	newTestAgent(t, newScript().Model(), WithConfig(cfg))
	if uid, gid, mode := ownerOf(t, toolHome); uid != freeID || gid != freeID || mode.Perm() != 0o700 {
		t.Fatalf("a home with the mode 0755: %d:%d %v", uid, gid, mode)
	}

	bad := DefaultConfig()
	bad.Home = home
	bad.Tools.Files.Root = ws
	bad.Tools.User = cfg.Tools.User
	bad.Tools.UserHome = ws
	if _, err := New(WithConfig(bad), WithModel(newScript().Model())); err == nil || !strings.Contains(err.Error(), "tools.userHome") {
		t.Fatalf("a home that holds FW_HOME: %v", err)
	}
}
