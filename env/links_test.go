//go:build unix

package env

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/bdubs00/featherweight-kernel/env/pkglist"
)

// A tool user can put a link in place of a file or a directory that it
// owns under dir. The writes and the reads of fw never follow such a link
// out of dir, and a directory of a manifest that is a link becomes a real
// directory again.
func TestLinksOfTheToolUser(t *testing.T) {
	f := newFixture(t, "packages:\n  python: [pyyaml]\n  tools: [jq]\n")
	s := f.s
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("token-of-fw"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The directory of pixi is a link out of dir.
	if err := os.Symlink(outside, s.pixiHome()); err != nil {
		t.Fatal(err)
	}
	if err := s.writeText(filepath.Join(s.pixiHome(), "config.toml"), "x"); err == nil {
		t.Fatal("a write followed a link out of dir")
	}
	if _, err := os.Stat(filepath.Join(outside, "config.toml")); err == nil {
		t.Fatal("the write landed out of dir")
	}

	// The directory of a manifest is a link: the install makes it a real
	// directory again, under a directory of fw.
	src := s.srcDir(pkglist.Python)
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, src); err != nil {
		t.Fatal(err)
	}
	in := newInstaller(nil, f.cfg, s)
	if err := in.makeSrc(src); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(src); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("src = %v, %v", info, err)
	}

	// The manifest is a link to a file of fw: readBack does not read it.
	if err := os.Symlink(secret, s.python); err != nil {
		t.Fatal(err)
	}
	if b := s.readBack(s.python); b != nil {
		t.Fatalf("readBack read %q through a link", b)
	}
	if err := s.writeText(s.python, "[project]\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(secret); string(b) != "token-of-fw" {
		t.Fatal("a write through the link of the manifest changed its target")
	}
	if info, err := os.Lstat(s.python); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the manifest = %v, %v", info, err)
	}

	// A link at the old fixed name of a temporary state file leads
	// nowhere, and a link in place of the lock file is refused.
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, s.markerPath()+".tmp"+strconv.Itoa(os.Getpid())); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(s.markerPath(), &Marker{Digest: "sha256:1"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatal("a state file followed a link")
	}
	created := filepath.Join(outside, "created")
	if err := os.Symlink(created, s.lockPath()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := tryLock(s.lockPath()); err == nil || ok {
		t.Fatalf("tryLock through a link: %v %v", ok, err)
	}
	if _, err := os.Stat(created); err == nil {
		t.Fatal("the lock created a file through a link")
	}
	if locked(s.lockPath()) {
		t.Fatal("a link counts as a held lock")
	}
}
