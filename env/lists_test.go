//go:build unix

package env

import (
	"slices"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/env/pkglist"
)

const listBlock = `
packages:
  python: [ansible-lint, " pyyaml>=6 "]
  tools: ["nodejs>=22", jq]
mirrors:
  python: http://pkg-cache.fw-fleet.svc.cluster.local:8080/pypi/simple/
  tools: http://pkg-cache.fw-fleet.svc.cluster.local:8080/conda-forge
`

// The keys packages and mirrors resolve to the lists and the mirrors of
// the spec, and a list or a mirror that does not fit stops the start.
func TestLoadLists(t *testing.T) {
	f := newFixture(t, listBlock)
	s := f.s
	if s == nil {
		t.Fatal("the block did not load")
	}
	if !slices.Equal(s.inline[pkglist.Python], []string{"ansible-lint", "pyyaml>=6"}) || !slices.Equal(s.inline[pkglist.Tools], []string{"nodejs>=22", "jq"}) {
		t.Fatalf("inline = %v", s.inline)
	}
	if s.mirrors[pkglist.Python] != "http://pkg-cache.fw-fleet.svc.cluster.local:8080/pypi/simple" || s.mirrors[pkglist.Tools] != "http://pkg-cache.fw-fleet.svc.cluster.local:8080/conda-forge" {
		t.Fatalf("mirrors = %v", s.mirrors)
	}
	empty := newFixture(t, "packages:\n  python: []\n")
	if l, ok := empty.s.inline[pkglist.Python]; !ok || len(l) != 0 {
		t.Fatalf("an empty list: %v %v", l, ok)
	}
	if _, ok := empty.s.inline[pkglist.Tools]; ok {
		t.Fatal("an absent list is present")
	}
	long := "packages:\n  tools: [" + strings.Repeat("a,", pkglist.MaxItems) + "a]\n"
	for block, want := range map[string]string{
		"packages:\n  python: [six]\npython: pyproject.toml":      "packages.python and environment.python cannot both exist",
		"packages:\n  tools: [jq]\ntools: pixi.toml":              "packages.tools and environment.tools cannot both exist",
		"packages:\n  pythn: [six]":                               "environment.packages.pythn is not a key",
		"mirrors:\n  pip: http://x/simple":                        "environment.mirrors.pip is not a key",
		"packages:\n  python: [six, \"--index-url=http://evil\"]": "packages.python item 1 starts with a dash",
		"packages:\n  tools: [\"jq\\u0007\"]":                     "packages.tools item 0 holds a control character",
		"packages:\n  python: [" + strings.Repeat("a", 201) + "]": "longer than 200 characters",
		long: "holds 101 items; the limit is 100",
		"mirrors:\n  python: ftp://mirror/simple":                       "mirrors.python is not an http or an https URL",
		"mirrors:\n  tools: http://bot:s3cr3t@mirror/conda-forge":       "mirrors.tools holds a user or a password",
		"placement: image\ndir: /opt/env\npackages:\n  python: [six]\n": "needs placement durable",
	} {
		g := newFixture(t, block)
		_, err := load(g.cfg)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", block, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%q: the error holds the password", block)
		}
	}
}
