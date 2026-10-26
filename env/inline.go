package env

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/env/pkglist"
)

// The lists of environment.packages take the place of the manifests. The
// kernel writes the manifest of each list under <dir>/src/<manager>, and
// the manager writes the lock file next to it. The kernel owns both: it
// writes the manifest again at each install, and the digest does not
// cover either file. The tool env records its changes in the file
// .fw-env-local.json, and the effective list of a manager is the
// configured list with those changes.

// localFile is the name of the record of the changes of the tool env.
const localFile = ".fw-env-local.json"

// indexFile is the name of the record of the mirror of each lock file
// that the kernel made.
const indexFile = ".fw-env-index.json"

// inlineName is the name of the project in the manifests that the kernel
// writes. pixi names the directory of the environment after it.
const inlineName = "fw-environment"

// defaultIndex is the index of uv without a mirror. An install names it
// on purpose, so uv locks again when a mirror goes away.
const defaultIndex = "https://pypi.org/simple"

// condaForge is the channel of the tools, and the URL that a mirror of
// the tools replaces.
const condaForge = "https://conda.anaconda.org/conda-forge"

// Local is the file .fw-env-local.json under dir: the changes of the tool
// env to the configured lists. Configured is the digest of the configured
// lists. When the configured lists change, the file no longer counts,
// because the front writes the approved changes into the configured lists
// of the pool.
type Local struct {
	Configured string   `json:"configured"`
	Changes    []Change `json:"changes"`
}

// Change is one add or remove of the tool env.
type Change struct {
	Manager string    `json:"manager"`
	Command string    `json:"command"`
	Package string    `json:"package"`
	Time    time.Time `json:"time"`
	Run     string    `json:"run,omitempty"`
	Agent   string    `json:"agent,omitempty"`
}

// isInline reports whether the list of environment.packages names the
// packages of manager.
func (s *spec) isInline(manager string) bool {
	_, ok := s.inline[manager]
	return ok
}

func (s *spec) srcDir(manager string) string { return filepath.Join(s.dir, "src", manager) }
func (s *spec) localPath() string            { return filepath.Join(s.dir, localFile) }
func (s *spec) indexPath() string            { return filepath.Join(s.dir, indexFile) }

// mirrorKey names the index of manager in the record of the lock files.
func (s *spec) mirrorKey(manager string) string {
	if m := s.mirrors[manager]; m != "" {
		return m
	}
	return "default"
}

// freshLock removes the lock file of manager when the kernel made it for
// another mirror, or when no record names its mirror, so the manager
// locks again from scratch. uv.lock holds the absolute URL of the mirror
// for each file. With an old URL, the install goes to a host that the
// Task cannot reach.
func (s *spec) freshLock(manager, lock string) error {
	var made map[string]string
	if s.readOwned(s.indexPath(), &made) && made[manager] == s.mirrorKey(manager) {
		return nil
	}
	return removeFile(lock)
}

// recordLock records the mirror of the lock file of manager.
func (s *spec) recordLock(manager string) error {
	made := map[string]string{}
	if !s.readOwned(s.indexPath(), &made) || made == nil {
		made = map[string]string{}
	}
	made[manager] = s.mirrorKey(manager)
	return writeFile(s.indexPath(), made)
}

// listsDigest returns the sha256 of the base: the switch on and the base
// lists, sorted, with a label for each manager. The file of the local
// changes names it, so a change of the base drops the local changes.
func (s *spec) listsDigest() string {
	h := sha256.New()
	fmt.Fprintf(h, "on %t\n", s.on)
	for _, m := range []string{pkglist.Python, pkglist.Tools} {
		l, ok := s.inline[m]
		if !ok {
			continue
		}
		sorted := slices.Clone(l)
		slices.Sort(sorted)
		fmt.Fprintf(h, "%s %d\n", m, len(sorted))
		for _, req := range sorted {
			fmt.Fprintf(h, "%s\n", req)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// readLocal reads the local changes. It returns nil when the file is
// absent, when the user of fw does not own it while a tool user exists,
// or when it names other configured lists.
func (s *spec) readLocal() *Local {
	if s.inline == nil {
		return nil
	}
	var l Local
	if !s.readOwned(s.localPath(), &l) || l.Configured != s.listsDigest() {
		return nil
	}
	return &l
}

// effective returns the list of manager: the configured list with the
// local changes. It returns nil for a manager without a list.
func (s *spec) effective(manager string) []string {
	l, ok := s.inline[manager]
	if !ok {
		return nil
	}
	out := slices.Clone(l)
	if out == nil {
		out = []string{}
	}
	if loc := s.readLocal(); loc != nil {
		for _, c := range loc.Changes {
			if c.Manager != manager || pkglist.CheckRequirement(manager, strings.TrimSpace(c.Package)) != nil {
				// The tool env checks each change before it keeps it, so
				// a change that fails the check is not of fw.
				continue
			}
			if next, err := applyChange(manager, out, c.Command, c.Package); err == nil {
				out = next
			}
		}
	}
	return out
}

// applyChange returns list after an add or a remove of pkg. An add takes
// the place of a requirement of the same package, so "pyyaml>=6" replaces
// "pyyaml". A remove takes out every requirement of the package, and it
// fails when the list holds none.
func applyChange(manager string, list []string, command, pkg string) ([]string, error) {
	name := pkglist.Name(manager, pkg)
	if name == "" {
		return nil, fmt.Errorf("%q names no package", pkg)
	}
	out := make([]string, 0, len(list)+1)
	found := false
	for _, req := range list {
		if pkglist.Name(manager, req) == name {
			if command == "add" && req == pkg {
				// The list holds this requirement already.
				return slices.Clone(list), nil
			}
			found = true
			continue
		}
		out = append(out, req)
	}
	switch command {
	case "add":
		if len(out)+1 > pkglist.MaxItems {
			return nil, fmt.Errorf("the list holds %d packages, the limit", pkglist.MaxItems)
		}
		return append(out, pkg), nil
	case "remove":
		if !found {
			return nil, fmt.Errorf("the list holds no package %s", name)
		}
		return out, nil
	}
	return nil, fmt.Errorf("the command %q is not add or remove", command)
}

// inlineDigest writes the part of the digest for a list: the effective
// list in sorted order.
func (s *spec) inlineDigest(part func(string, []byte), manager string) {
	l := slices.Clone(s.effective(manager))
	slices.Sort(l)
	part(manager+" list", []byte(strings.Join(l, "\n")))
}

// condaPlatform returns the conda name of the platform of this machine,
// such as linux-64.
func condaPlatform() string {
	arch := map[string]string{"amd64": "64", "arm64": "aarch64", "ppc64le": "ppc64le", "s390x": "s390x"}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	switch runtime.GOOS {
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "osx-arm64"
		}
		return "osx-" + arch
	case "windows":
		return "win-" + arch
	}
	return runtime.GOOS + "-" + arch
}

// pyproject returns the manifest of uv for a list. requires-python names
// the minor version of the Python of the image, so uv resolves for that
// interpreter alone.
func pyproject(list []string, pythonMinor string) string {
	var b strings.Builder
	b.WriteString("# Written by fw from environment.packages.python. fw writes this file again\n# at each install, so a change here does not last.\n")
	fmt.Fprintf(&b, "[project]\nname = %s\nversion = \"0.0.0\"\n", tomlString(inlineName))
	fmt.Fprintf(&b, "requires-python = %s\n", tomlString("=="+pythonMinor+".*"))
	b.WriteString("dependencies = [\n")
	sorted := slices.Clone(list)
	slices.Sort(sorted)
	for _, req := range sorted {
		fmt.Fprintf(&b, "  %s,\n", tomlString(req))
	}
	b.WriteString("]\n")
	return b.String()
}

// pixiManifest returns the manifest of pixi for a list: the channel
// conda-forge and the platform of this machine. Each requirement is a
// name, optionally with a version and a build, such as "nodejs>=22",
// "nodejs 22.*", or "openssl 3.* *_0". A requirement in another form, or
// two requirements of one package, fail with the reason.
func pixiManifest(list []string) (string, error) {
	deps := map[string]condaSpec{}
	for _, req := range list {
		d, err := splitConda(req)
		if err != nil {
			return "", err
		}
		key := strings.ToLower(d.name)
		if _, dup := deps[key]; dup {
			return "", fmt.Errorf("the list names the package %s twice", d.name)
		}
		deps[key] = d
	}
	var b strings.Builder
	b.WriteString("# Written by fw from environment.packages.tools. fw writes this file again\n# at each install, so a change here does not last.\n")
	fmt.Fprintf(&b, "[workspace]\nname = %s\nchannels = [\"conda-forge\"]\nplatforms = [%s]\n\n[dependencies]\n", tomlString(inlineName), tomlString(condaPlatform()))
	for _, key := range slices.Sorted(maps.Keys(deps)) {
		d := deps[key]
		if d.build == "" {
			fmt.Fprintf(&b, "%s = %s\n", tomlString(d.name), tomlString(d.version))
		} else {
			fmt.Fprintf(&b, "%s = { version = %s, build = %s }\n", tomlString(d.name), tomlString(d.version), tomlString(d.build))
		}
	}
	return b.String(), nil
}

// condaSpec is a requirement of a list of tools.
type condaSpec struct{ name, version, build string }

// splitConda splits a requirement of a list of tools into the name, the
// version, and the build. A missing version is "*".
func splitConda(req string) (condaSpec, error) {
	req = strings.TrimSpace(req)
	end := 0
	for end < len(req) && (isAlnum(req[end]) || strings.IndexByte("-_.", req[end]) >= 0) {
		end++
	}
	if end == 0 || !isAlnum(req[0]) {
		return condaSpec{}, fmt.Errorf("%q does not start with the name of a package", req)
	}
	d := condaSpec{name: req[:end], version: "*"}
	rest := strings.TrimSpace(req[end:])
	if rest == "" {
		return d, nil
	}
	fields := strings.Fields(rest)
	if len(fields) > 2 {
		return condaSpec{}, fmt.Errorf("%q is not a name with a version and a build, such as \"nodejs>=22\" or \"nodejs 22.*\"", req)
	}
	for _, r := range fields[0] {
		if !(r < 0x80 && (isAlnum(byte(r)) || strings.ContainsRune("<>=!~*.,|_+-", r))) {
			return condaSpec{}, fmt.Errorf("%q is not a name with a version, such as \"nodejs>=22\" or \"nodejs 22.*\"; a channel or a bracket is not available", req)
		}
	}
	d.version = fields[0]
	if len(fields) == 2 {
		d.build = fields[1]
	}
	return d, nil
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// tomlString quotes s as a basic string of TOML. A JSON string with HTML
// escapes off is a valid basic string of TOML for the characters that a
// list can hold.
func tomlString(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// pixiConfig returns the configuration of pixi that fw writes into
// PIXI_HOME: the directory of the environments, and the mirror of
// conda-forge.
func (s *spec) pixiConfig() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by fw (environment). Each environment lives under %s.\ndetached-environments = %s\n", s.dir, tomlString(s.pixiEnvs()))
	if m := s.mirrors[pkglist.Tools]; m != "" {
		fmt.Fprintf(&b, "\n[mirrors]\n%s = [%s]\n", tomlString(condaForge), tomlString(m))
	}
	return b.String()
}

// inDir opens dir as an os.Root and returns the path of p in it. A path
// of the root cannot lead out of dir, also through a link that the tool
// user put in place of a directory below dir.
func (s *spec) inDir(p string) (*os.Root, string, error) {
	rel, err := filepath.Rel(s.dir, p)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, "", fmt.Errorf("%s is not under %s", p, s.dir)
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, "", err
	}
	return root, rel, nil
}

// writeText writes a file under dir, in a directory that the tool user
// can write. It writes a new temporary file with a random name and
// renames it, so a link that the tool user put in place of the file is
// replaced and not followed, and a link in place of a directory cannot
// lead the write out of dir (inDir).
func (s *spec) writeText(path, text string) error {
	root, rel, err := s.inDir(path)
	if err != nil {
		return err
	}
	defer root.Close()
	tmp := filepath.Join(filepath.Dir(rel), "."+filepath.Base(rel)+".tmp"+randomSuffix())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		_ = root.Remove(tmp)
	}
	return err
}

// randomSuffix returns 16 random hex digits for the name of a temporary
// file.
func randomSuffix() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// readBack returns the content of a file under dir, or nil when it is
// absent or leads out of dir, so a failed lock can put the manifest
// back. The read never follows a link out of dir, so a link that the
// tool user put in place of the manifest cannot copy a file of fw.
func (s *spec) readBack(path string) []byte {
	root, rel, err := s.inDir(path)
	if err != nil {
		return nil
	}
	defer root.Close()
	b, err := root.ReadFile(rel)
	if err != nil {
		return nil
	}
	return b
}

// putBack writes the content that readBack returned, or removes the file
// when it was absent.
func (s *spec) putBack(path string, b []byte) error {
	if b == nil {
		root, rel, err := s.inDir(path)
		if err != nil {
			return err
		}
		defer root.Close()
		if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return s.writeText(path, string(b))
}

// claimDir makes rel, a directory under dir, a real directory of fw with
// the mode 0755. A link or a file in its place goes first. With a tool
// user, fw takes the directory back, because the walk of the workspace
// at the start gives every file to the tool user.
func (s *spec) claimDir(rel string) error {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if info, err := root.Lstat(rel); err == nil && (info.Mode()&fs.ModeSymlink != 0 || !info.IsDir()) {
		if err := root.Remove(rel); err != nil {
			return err
		}
	}
	if err := root.MkdirAll(rel, 0o755); err != nil {
		return err
	}
	if s.user != nil && os.Geteuid() == 0 {
		if err := root.Lchown(rel, os.Geteuid(), os.Getegid()); err != nil {
			return err
		}
		if err := root.Chmod(rel, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// toolDir makes rel, a directory under dir, a real directory of the tool
// user, for a manager. A link or a file in its place goes first.
func (s *spec) toolDir(rel string) error {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if info, err := root.Lstat(rel); err == nil && (info.Mode()&fs.ModeSymlink != 0 || !info.IsDir()) {
		if err := root.Remove(rel); err != nil {
			return err
		}
	}
	if err := root.MkdirAll(rel, 0o755); err != nil {
		return err
	}
	if s.user != nil && os.Geteuid() == 0 {
		return root.Lchown(rel, int(s.user.UID), int(s.user.GID))
	}
	return nil
}

// writeLocal writes the file of the local changes.
func (s *spec) writeLocal(l *Local) error {
	return writeFile(s.localPath(), l)
}

// dropStaleLocal removes a file of local changes that names other
// configured lists, or that a block without lists left. The caller holds
// the lock.
func (s *spec) dropStaleLocal() error {
	if s.readLocal() != nil {
		return nil
	}
	if _, err := os.Lstat(s.localPath()); err != nil {
		return nil
	}
	return removeFile(s.localPath())
}

// pythonMinor parses the output of `python3 --version`, such as
// "Python 3.11.2", into "3.11".
func pythonMinor(out string) (string, error) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 2 || f[0] != "Python" {
		return "", fmt.Errorf("python3 --version said %q", firstLine(out))
	}
	parts := strings.SplitN(f[1], ".", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("python3 --version said %q", firstLine(out))
	}
	for _, p := range parts[:2] {
		if _, err := strconv.Atoi(p); err != nil {
			return "", fmt.Errorf("python3 --version said %q", firstLine(out))
		}
	}
	return parts[0] + "." + parts[1], nil
}
