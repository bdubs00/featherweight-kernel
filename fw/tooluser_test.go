//go:build unix

package fw

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// A uid and a gid that no test system lists in /etc/passwd.
const freeID = 54321

func TestLookupToolUserNone(t *testing.T) {
	for _, spec := range []string{"", "  ", ToolUserNone} {
		if u, err := LookupToolUser(spec); u != nil || err != nil {
			t.Errorf("LookupToolUser(%q) = %v, %v; want nil, nil", spec, u, err)
		}
	}
}

func TestLookupToolUserRefuses(t *testing.T) {
	for spec, want := range map[string]string{
		"root":                      "is root",
		"0:0":                       "is root",
		"0":                         "is root",
		strconv.Itoa(freeID) + ":0": "is root",
		"fw-no-such-user-here":      "does not exist",
		strconv.Itoa(freeID):        "does not list",
		":5":                        "is not a user",
		"5:":                        "is not a user",
		"fw-no-such-user-here:5":    "does not exist",
	} {
		u, err := LookupToolUser(spec)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "tools.user") {
			t.Errorf("LookupToolUser(%q) = %v, %v; want an error with %q", spec, u, err, want)
		}
	}
}

func TestLookupToolUserNumbers(t *testing.T) {
	spec := strconv.Itoa(freeID) + ":" + strconv.Itoa(freeID+1)
	u, err := LookupToolUser(spec)
	if err != nil {
		t.Fatal(err)
	}
	if u.UID != freeID || u.GID != freeID+1 || u.Name != "" || u.Home != "" || u.Spec != spec {
		t.Fatalf("u = %+v", u)
	}
	if got := u.String(); got != "uid 54321 (54321:54322)" {
		t.Errorf("String = %q", got)
	}
}

func TestLookupToolUserByName(t *testing.T) {
	me, err := user.Current()
	if err != nil || me.Uid == "0" {
		t.Skip("needs a user that is not root")
	}
	u, err := LookupToolUser(me.Username)
	if err != nil {
		t.Fatal(err)
	}
	if strconv.FormatUint(uint64(u.UID), 10) != me.Uid || strconv.FormatUint(uint64(u.GID), 10) != me.Gid || u.Home != me.HomeDir {
		t.Fatalf("u = %+v, want %+v", u, me)
	}
	byID, err := LookupToolUser(me.Uid + ":" + strconv.Itoa(freeID))
	if err != nil || byID.UID != u.UID || byID.GID != freeID || byID.Name != me.Username {
		t.Fatalf("uid:gid = %+v, %v", byID, err)
	}
}

func TestToolUserApply(t *testing.T) {
	var none *ToolUser
	cmd := exec.Command("true")
	none.Apply(cmd)
	if cmd.SysProcAttr != nil || cmd.Env != nil {
		t.Fatal("a nil user changed the command")
	}
	u := &ToolUser{Name: "nonroot", UID: 65532, GID: 65532, Home: "/home/nonroot"}
	cmd = exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = []string{"HOME=/root", "PATH=/bin"}
	u.Apply(cmd)
	c := cmd.SysProcAttr.Credential
	if !cmd.SysProcAttr.Setpgid || c == nil || c.Uid != 65532 || c.Gid != 65532 || c.Groups == nil || len(c.Groups) != 0 || c.NoSetGroups {
		t.Fatalf("SysProcAttr = %+v, Credential = %+v", cmd.SysProcAttr, c)
	}
	if v, _ := envValue(cmd.Env, "HOME"); v != "/home/nonroot" {
		t.Errorf("HOME = %q", v)
	}
	if v, _ := envValue(cmd.Env, "USER"); v != "nonroot" {
		t.Errorf("USER = %q", v)
	}
	bare := &ToolUser{UID: freeID, GID: freeID}
	cmd = exec.Command("true")
	bare.Apply(cmd)
	if v, _ := envValue(cmd.Env, "HOME"); v != "/" {
		t.Errorf("HOME without an entry = %q", v)
	}
	if v, _ := envValue(cmd.Env, "LOGNAME"); v != strconv.Itoa(freeID) {
		t.Errorf("LOGNAME without an entry = %q", v)
	}
	if _, ok := envValue(cmd.Env, "PATH"); !ok {
		t.Error("a nil environment did not become the environment of fw")
	}
}

// A process that is not root refuses a tool user, and New stops with an
// error that says what to set.
func TestToolUserNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runs as root")
	}
	if err := canBecome(&ToolUser{UID: uint32(os.Geteuid())}); err == nil || !strings.Contains(err.Error(), "user of fw itself") {
		t.Errorf("same user: %v", err)
	}
	cfg := DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Files.Root = t.TempDir()
	cfg.Tools.User = strconv.Itoa(freeID) + ":" + strconv.Itoa(freeID)
	_, err := New(WithConfig(cfg), WithoutDefaults(), WithAuthn(NoAuthn{}), WithModel(newScript().Model()), WithLedger(NewMemLedger()), WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "run fw as root, or set tools.user: none") {
		t.Fatalf("New = %v", err)
	}
	cfg.Tools.User = "fw-no-such-user-here"
	_, err = New(WithConfig(cfg), WithoutDefaults(), WithAuthn(NoAuthn{}), WithModel(newScript().Model()), WithLedger(NewMemLedger()), WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("New with a missing user = %v", err)
	}
}

func TestToolRootsAndGuards(t *testing.T) {
	ws := t.TempDir()
	cfg := DefaultConfig()
	cfg.Tools.Files.Root = filepath.Join(ws, "app")
	cfg.Workspaces = []string{ws, filepath.Join(ws, "app"), filepath.Join(ws, "other")}
	roots, err := toolRoots(cfg)
	if err != nil {
		t.Fatal(err)
	}
	real := evalExisting(ws)
	if !slices.Equal(roots, []string{real}) {
		t.Fatalf("roots = %v, want the volume alone", roots)
	}
	reserved := []string{filepath.Join(real, ".fw"), filepath.Join(real, ".fw", "fw.yaml"), filepath.Join(real, "cfg", "deep", "fw.yaml"), "/elsewhere/ledger"}
	got := guardDirs(roots, reserved, []string{filepath.Join(real, "app")})
	want := []string{real, filepath.Join(real, "app"), filepath.Join(real, "cfg"), filepath.Join(real, "cfg", "deep")}
	if !slices.Equal(got, want) {
		t.Fatalf("guards = %v, want %v", got, want)
	}
	rec := treeRecord(&ToolUser{UID: 7, GID: 8}, roots, reserved)
	if !strings.HasPrefix(rec, "user 7:8\nroot "+real+"\nreserved ") {
		t.Fatalf("record = %q", rec)
	}
	if !under(filepath.Join(real, "a"), real) || !under(real, real) || under(real+"x", real) || under("/", real) {
		t.Error("under is wrong")
	}
}

// The walk gives every entry to the tool user except the reserved paths
// and the guard directories, never follows a link, and keeps a file with
// more than one hard link.
func TestChownTree(t *testing.T) {
	root := evalExisting(t.TempDir())
	for _, d := range []string{"app/src", ".fw/runs", "cfg"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"app/src/main.go", ".fw/fw.yaml", "cfg/fw.yaml", "cfg/other", "linked"} {
		mustWrite(t, filepath.Join(root, f), "x")
	}
	if err := os.Link(filepath.Join(root, "linked"), filepath.Join(root, "app", "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "app", "etc")); err != nil {
		t.Fatal(err)
	}
	reserved := []string{filepath.Join(root, ".fw"), filepath.Join(root, "cfg", "fw.yaml")}
	guards := guardDirs([]string{root}, reserved, nil)
	var got []string
	u := &ToolUser{UID: freeID, GID: freeID}
	keep := func(p string, _ bool) bool { return slices.Contains(guards, p) }
	changed, kept, err := chownTree(root, u, reserved, keep, func(p string, uid, gid int) error {
		if uid != freeID || gid != freeID {
			t.Errorf("lchown %s to %d:%d", p, uid, gid)
		}
		rel, _ := filepath.Rel(root, p)
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app", "app/etc", "app/src", "app/src/main.go", "cfg/other"}
	if !slices.Equal(got, want) || changed != len(want) || kept != 2 {
		t.Fatalf("changed %v (%d), kept %d; want %v and 2 linked files", got, changed, kept, want)
	}
}

// allows and blocked read the owner, the group, and the mode of a path as
// the kernel checks them for a user without other groups.
func TestToolUserPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every mode the same way, and a test of other users needs a user that is not root")
	}
	dir := t.TempDir()
	// The test directory and its parent belong to this user with 0700,
	// so open them for the other bits.
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	other := &ToolUser{UID: freeID, GID: freeID}
	self := &ToolUser{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	file := filepath.Join(dir, "token")
	mustWrite(t, file, "secret")
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(file)
	if other.allows(info, 0o4) || !self.allows(info, 0o4) || !self.allows(info, 0o6) {
		t.Error("allows is wrong for a file of mode 0600")
	}
	if why := other.blocked(file, 0o4); !strings.Contains(why, "mode 600") {
		t.Errorf("blocked = %q", why)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if why := other.blocked(file, 0o4); why != "" {
		t.Errorf("a file of mode 0644 is blocked: %q", why)
	}
	if why := other.blocked(dir, 0o3); !strings.Contains(why, "mode 755") {
		t.Errorf("blocked write = %q", why)
	}

	// A user that writes the directory of a path can rename it, unless the
	// directory is sticky and neither belongs to the user.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if x := other.canRename(file); x != file {
		t.Errorf("canRename in a directory of mode 0777 = %q", x)
	}
	if err := os.Chmod(dir, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if x := other.canRename(file); x != "" {
		t.Errorf("canRename in a sticky directory = %q", x)
	}
	if x := self.canRename(file); x != file {
		t.Errorf("the owner cannot rename in its own sticky directory: %q", x)
	}
}

// ToolUserOf gives the tool user tools.userHome as its home, so every
// process of that user gets the same HOME. A relative home is an error,
// and without a tool user the home does not matter.
func TestToolUserOf(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tools.User = "54321:54321"
	u, err := ToolUserOf(cfg)
	if err != nil || u == nil || u.Home != "" {
		t.Fatalf("ToolUserOf = %+v, %v", u, err)
	}
	cfg.Tools.UserHome = "/workspace/.home/"
	if u, err = ToolUserOf(cfg); err != nil || u.Home != "/workspace/.home" {
		t.Fatalf("ToolUserOf = %+v, %v", u, err)
	}
	cmd := exec.Command("true")
	cmd.Env = []string{"HOME=/root"}
	u.Apply(cmd)
	if got := cmd.Env[len(cmd.Env)-3]; got != "HOME=/workspace/.home" {
		t.Fatalf("env = %v", cmd.Env)
	}
	cfg.Tools.UserHome = "home"
	if _, err := ToolUserOf(cfg); err == nil || !strings.Contains(err.Error(), "tools.userHome \"home\" is not an absolute path") {
		t.Fatalf("relative home: %v", err)
	}
	cfg.Tools.User = ToolUserNone
	if u, err := ToolUserOf(cfg); u != nil || err != nil {
		t.Fatalf("no tool user: %v, %v", u, err)
	}
	if u, err := ToolUserOf(nil); u != nil || err != nil {
		t.Fatalf("nil config: %v, %v", u, err)
	}
}
