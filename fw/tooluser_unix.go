//go:build unix

package fw

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

const toolUserSupported = true

// Apply makes cmd start as u: with its uid, its gid, and no other group,
// and with HOME, USER, and LOGNAME of u at the end of the environment,
// where they win over the same names before them. A nil cmd.Env becomes
// the environment of fw first, so pass the environment of a tool process
// (ToolEnv) in cmd.Env. A nil u leaves cmd as it is.
func (u *ToolUser) Apply(cmd *exec.Cmd) {
	if u == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: u.UID, Gid: u.GID, Groups: []uint32{}}
	home, name := u.Home, u.Name
	if home == "" {
		home = "/"
	}
	if name == "" {
		name = strconv.FormatUint(uint64(u.UID), 10)
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "HOME="+home, "USER="+name, "LOGNAME="+name)
}

// canBecome reports why this process cannot start a process as u: only
// root can, and a tool user that is the user of fw keeps nothing from the
// tools.
func canBecome(u *ToolUser) error {
	euid := os.Geteuid()
	switch {
	case euid == 0:
		return nil
	case uint32(euid) == u.UID:
		return fmt.Errorf("fw: tools.user %s is the user of fw itself, so the tools can read the home directory and the memory of fw; run fw as root, or set tools.user: none", u)
	default:
		return fmt.Errorf("fw: fw runs as uid %d and cannot start a process as tools.user %s; run fw as root, or set tools.user: none", euid, u)
	}
}

// guardMode is the mode of a directory of the workspace that holds the
// state of fw: owned by fw, writable by the group of the tool user, and
// sticky, so the tool user adds files there but never renames or removes
// an entry of fw, such as FW_HOME.
const guardMode = os.ModeDir | os.ModeSticky | 0o775

// useToolUser prepares the files for the tool user u at New (section 7.12):
//
//  1. The home directory of fw and the directory of the file ledger belong
//     to fw and have mode 0700, so the tool user can neither read nor
//     write the ledger, the configuration, the memory, or the profiles.
//  2. Once, behind ToolTreeMarker, every file of the workspace except those
//     paths changes its owner to the tool user, so a tool can change the
//     files that the boot of ax cloned. A directory on the way from the
//     root of the workspace to a reserved path, and the root of the files
//     tool with the directories above it, stay with fw instead, and get
//     guardMode at every start.
//  3. The tool user must reach each root of the workspace, cannot read the
//     file of the API token, and cannot change the configuration file.
func (a *Agent) useToolUser(u *ToolUser) error {
	if err := canBecome(u); err != nil {
		return err
	}
	cfg := a.cfg
	uid, gid := os.Geteuid(), os.Getegid()
	private := []string{cfg.Home}
	if cfg.Ledger.Path != "" {
		private = append(private, cfg.Ledger.Path)
	}
	for _, dir := range private {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("fw: tools.user: the directory %s of fw: %w", dir, err)
		}
		if err := os.Chown(dir, uid, gid); err != nil {
			return fmt.Errorf("fw: tools.user: the directory %s of fw: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("fw: tools.user: the directory %s of fw: %w", dir, err)
		}
	}
	reserved := make([]string, 0, 3)
	for _, p := range cfg.Reserved() {
		if abs, err := filepath.Abs(p); err == nil {
			reserved = append(reserved, evalExisting(abs))
		}
	}
	roots, err := toolRoots(cfg)
	if err != nil {
		return err
	}
	for _, r := range roots {
		for _, p := range reserved {
			if under(r, p) {
				return fmt.Errorf("fw: tools.user: the workspace %s lies in %s, which holds the state or the configuration of fw; move tools.files.root out of it", r, p)
			}
		}
		if err := os.MkdirAll(r, 0o755); err != nil {
			return fmt.Errorf("fw: tools.user: the workspace %s: %w", r, err)
		}
	}
	// The root of the files tool stays with fw too: a tool user that can
	// rename it can put a link to FW_HOME in its place, and the files tool
	// opens the root by its name at each call.
	filesRoot, err := filepath.Abs(cfg.WorkspaceRoot())
	if err != nil {
		return fmt.Errorf("fw: tools.user: the workspace %s: %w", cfg.WorkspaceRoot(), err)
	}
	filesRoot = evalExisting(filesRoot)
	guards := guardDirs(roots, reserved, []string{filesRoot})
	marker := ToolTreeMarker(cfg.Home)
	want := treeRecord(u, roots, reserved)
	if have, err := os.ReadFile(marker); err != nil || string(have) != want {
		for _, r := range roots {
			n, kept, err := chownTree(r, u, reserved, guards, os.Lchown)
			if err != nil {
				return fmt.Errorf("fw: tools.user: give the workspace %s to %s: %w", r, u, err)
			}
			a.log.Info("the tool user owns the workspace", "root", r, "user", u.String(), "changed", n, "keptHardLinks", kept)
		}
		if err := os.WriteFile(marker, []byte(want), 0o600); err != nil {
			return fmt.Errorf("fw: tools.user: %w", err)
		}
	}
	for _, d := range guards {
		if err := os.Chown(d, uid, int(u.GID)); err != nil {
			return fmt.Errorf("fw: tools.user: the directory %s, which holds the state of fw: %w", d, err)
		}
		if err := os.Chmod(d, guardMode); err != nil {
			return fmt.Errorf("fw: tools.user: the directory %s, which holds the state of fw: %w", d, err)
		}
	}
	if cfg.ConfigFile != "" {
		abs, err := filepath.Abs(cfg.ConfigFile)
		if err != nil {
			return fmt.Errorf("fw: tools.user: the configuration file %s: %w", cfg.ConfigFile, err)
		}
		abs = evalExisting(abs)
		if slices.ContainsFunc(roots, func(r string) bool { return under(abs, r) }) {
			// A file of the workspace, such as ./fw.yaml: only fw reads it.
			if err := os.Chown(abs, uid, gid); err != nil {
				return fmt.Errorf("fw: tools.user: the configuration file %s: %w", abs, err)
			}
			if err := os.Chmod(abs, 0o600); err != nil {
				return fmt.Errorf("fw: tools.user: the configuration file %s: %w", abs, err)
			}
		} else if info, err := os.Stat(abs); err == nil && u.allows(info, 0o2) {
			return fmt.Errorf("fw: tools.user %s can write the configuration file %s; make it owned by fw with mode 0600", u, abs)
		}
	}
	for _, p := range append(slices.Clone(reserved), filesRoot) {
		if x := u.canRename(p); x != "" {
			return fmt.Errorf("fw: tools.user %s can rename %s, and with it %s, which holds the state or the configuration of fw; make %s writable by fw alone", u, x, p, filepath.Dir(x))
		}
	}
	for _, r := range roots {
		if !slices.Contains(guards, r) {
			if err := os.Lchown(r, int(u.UID), int(u.GID)); err != nil {
				return fmt.Errorf("fw: tools.user: the workspace %s: %w", r, err)
			}
		}
		if why := u.blocked(r, 0o3); why != "" {
			return fmt.Errorf("fw: tools.user %s cannot work in the workspace %s: %s; give that directory the mode 0755, or set tools.user: none", u, r, why)
		}
	}
	if err := u.checkTokenFile(cfg); err != nil {
		return err
	}
	if slices.ContainsFunc(reserved, func(p string) bool { return under(p, filesRoot) }) {
		a.log.Warn("the root of the files tool holds the state of fw: the files tool keeps out of it, but a shell that changes a path while the files tool opens it can lead the tool there; put FW_HOME outside the root for untrusted input", "home", cfg.Home, "root", filesRoot)
	}
	return nil
}

// toolRoots returns the directories of the workspace that the tool user
// owns: the root of the files tool and Config.Workspaces, absolute and
// after symbolic links, without a root that lies under another.
func toolRoots(cfg *Config) ([]string, error) {
	var all []string
	for _, p := range append([]string{cfg.WorkspaceRoot()}, cfg.Workspaces...) {
		if strings.TrimSpace(p) == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("fw: tools.user: the workspace %s: %w", p, err)
		}
		all = append(all, evalExisting(abs))
	}
	slices.Sort(all)
	all = slices.Compact(all)
	var out []string
	for _, p := range all {
		if !slices.ContainsFunc(out, func(r string) bool { return under(p, r) }) {
			out = append(out, p)
		}
	}
	return out, nil
}

// under reports whether p is dir or lies under it.
func under(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && filepath.IsLocal(rel) || p == dir
}

// guardDirs returns the directories that stay with fw (guardMode): each
// directory from a root down to the parent of a reserved path under that
// root, and each directory of keep with the directories above it up to its
// root. A reserved path in another one, such as fw.yaml in FW_HOME, needs
// no guard of its own, because the outer path keeps the tool user out.
func guardDirs(roots, reserved, keep []string) []string {
	var out []string
	chain := func(dir string) {
		for _, r := range roots {
			if !under(dir, r) {
				continue
			}
			for d := dir; ; d = filepath.Dir(d) {
				out = append(out, d)
				if d == r || d == filepath.Dir(d) {
					break
				}
			}
		}
	}
	for _, p := range reserved {
		if slices.ContainsFunc(reserved, func(q string) bool { return q != p && under(p, q) }) {
			continue
		}
		if !slices.Contains(roots, p) {
			chain(filepath.Dir(p))
		}
	}
	for _, k := range keep {
		chain(k)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// treeRecord is the content of ToolTreeMarker: the user, the roots, and
// the reserved paths. A change of any of them walks the workspace again.
func treeRecord(u *ToolUser, roots, reserved []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "user %d:%d\n", u.UID, u.GID)
	for _, r := range roots {
		fmt.Fprintf(&b, "root %s\n", r)
	}
	for _, r := range reserved {
		fmt.Fprintf(&b, "reserved %s\n", r)
	}
	return b.String()
}

// chownTree gives every entry under root to u, without following a
// symbolic link and without crossing into another file system. It skips
// each reserved path and leaves each guard directory to useToolUser. A
// file with more than one hard link keeps its owner, because a link can
// lead to a file outside the workspace. It returns how many entries it
// changed and how many linked files it kept. lchown is os.Lchown outside
// a test.
func chownTree(root string, u *ToolUser, reserved, guards []string, lchown func(string, int, int) error) (changed, kept int, err error) {
	top, err := os.Lstat(root)
	if err != nil {
		return 0, 0, err
	}
	dev := top.Sys().(*syscall.Stat_t).Dev
	uid, gid := int(u.UID), int(u.GID)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if slices.Contains(reserved, p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		if d.IsDir() && st.Dev != dev {
			return filepath.SkipDir
		}
		if slices.Contains(guards, p) {
			return nil
		}
		if !d.IsDir() && st.Nlink > 1 {
			kept++
			return nil
		}
		if st.Uid == u.UID && st.Gid == u.GID {
			return nil
		}
		if err := lchown(p, uid, gid); err != nil {
			return err
		}
		changed++
		return nil
	})
	return changed, kept, err
}

// allows reports whether u has the permission bits of want (4 read, 2
// write, 1 search) on a file by its owner, its group, and its mode. The
// tool user has no other group, so the check is exact without ACLs.
func (u *ToolUser) allows(info fs.FileInfo, want uint32) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	mode := uint32(info.Mode().Perm())
	switch {
	case st.Uid == u.UID:
		mode >>= 6
	case st.Gid == u.GID:
		mode >>= 3
	}
	return mode&want == want
}

// blocked returns why u cannot use the path p with the bits want, or "":
// every directory above p needs search, and p itself needs want.
func (u *ToolUser) blocked(p string, want uint32) string {
	p = filepath.Clean(p)
	var above []string
	for d := filepath.Dir(p); ; d = filepath.Dir(d) {
		above = append(above, d)
		if d == filepath.Dir(d) {
			break
		}
	}
	for i := len(above) - 1; i >= 0; i-- {
		info, err := os.Stat(above[i])
		if err != nil {
			return err.Error()
		}
		if !u.allows(info, 0o1) {
			return fmt.Sprintf("%s has the mode %o and the owner %d", above[i], info.Mode().Perm(), info.Sys().(*syscall.Stat_t).Uid)
		}
	}
	info, err := os.Stat(p)
	if err != nil {
		return err.Error()
	}
	if !u.allows(info, want) {
		return fmt.Sprintf("%s has the mode %o and the owner %d", p, info.Mode().Perm(), info.Sys().(*syscall.Stat_t).Uid)
	}
	return ""
}

// canRename returns the first path from p up to the root of the file
// system that u can rename or remove, or "". u can do that when it can
// write the directory of the path, unless that directory is sticky and
// neither the path nor the directory belongs to u.
func (u *ToolUser) canRename(p string) string {
	for x := filepath.Clean(p); x != filepath.Dir(x); x = filepath.Dir(x) {
		dir := filepath.Dir(x)
		if u.blocked(dir, 0o3) != "" {
			continue
		}
		di, err := os.Stat(dir)
		if err != nil {
			continue
		}
		xi, err := os.Lstat(x)
		if err != nil {
			continue
		}
		if di.Mode()&os.ModeSticky != 0 && xi.Sys().(*syscall.Stat_t).Uid != u.UID && di.Sys().(*syscall.Stat_t).Uid != u.UID {
			continue
		}
		return x
	}
	return ""
}

// checkTokenFile refuses a file of the API token that u can read
// (<auth.tokenEnv>_FILE, section 7.12).
func (u *ToolUser) checkTokenFile(cfg *Config) error {
	name := cfg.Auth.TokenEnv
	if name == "" {
		name = "FW_API_TOKEN"
	}
	p := os.Getenv(name + "_FILE")
	if p == "" {
		return nil
	}
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return nil // the token mode reports a token that is missing
	}
	if u.blocked(p, 0o4) == "" {
		return fmt.Errorf("fw: tools.user %s can read the token file %s of %s_FILE; make it owned by fw with mode 0400 or 0600", u, p, name)
	}
	return nil
}
