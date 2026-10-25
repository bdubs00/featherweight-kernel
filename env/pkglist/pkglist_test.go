package pkglist

import (
	"strings"
	"testing"
)

// A requirement that starts with a dash, holds a control character, or is
// too long fails, and so does a list that is too long.
func TestCheck(t *testing.T) {
	for _, ok := range []string{"pyyaml", "pyyaml>=6", "ansible-lint[yaml]", "nodejs 22.*", "conda-forge::jq", strings.Repeat("a", MaxLength)} {
		if err := CheckRequirement(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for req, want := range map[string]string{
		"":                               "is empty",
		"--index-url=http://evil":        "starts with a dash",
		"-e .":                           "starts with a dash",
		"pyyaml\n--pre":                  "control character",
		"a\tb":                           "control character",
		strings.Repeat("a", MaxLength+1): "longer than 200",
	} {
		if err := CheckRequirement(req); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", req, err, want)
		}
	}
	if err := Check([]string{" pyyaml ", "six"}); err != nil {
		t.Fatal(err)
	}
	if err := Check([]string{"six", " -x"}); err == nil || err.Error() != "item 1 starts with a dash" {
		t.Fatalf("err = %v", err)
	}
	if err := Check(make([]string, MaxItems+1)); err == nil || !strings.Contains(err.Error(), "holds 101 items; the limit is 100") {
		t.Fatalf("err = %v", err)
	}
	if err := Check(nil); err != nil {
		t.Fatal(err)
	}
}

// Name gives the name that compares two requirements of one manager.
func TestName(t *testing.T) {
	for _, c := range []struct{ manager, req, want string }{
		{Python, "PyYAML>=6", "pyyaml"},
		{Python, "ruamel.yaml", "ruamel-yaml"},
		{Python, "Ruamel__Yaml ; python_version > '3'", "ruamel-yaml"},
		{Python, "ansible-lint[yaml]==24.9.2", "ansible-lint"},
		{Python, "requests @ https://example.org/r.whl", "requests"},
		{Python, ">=6", ""},
		{Tools, "nodejs>=22", "nodejs"},
		{Tools, "NodeJS 22.*", "nodejs"},
		{Tools, "conda-forge::jq", "jq"},
		{Tools, "python_abi", "python_abi"},
		{Tools, "jq[version='>=1.7']", "jq"},
	} {
		if got := Name(c.manager, c.req); got != c.want {
			t.Errorf("Name(%s, %q) = %q, want %q", c.manager, c.req, got, c.want)
		}
	}
}

// A mirror is an http or an https URL with a host and nothing secret.
func TestCheckMirror(t *testing.T) {
	for _, ok := range []string{"http://pkg-cache.fw-fleet.svc.cluster.local:8080/pypi/simple", "https://mirror.example/conda-forge/"} {
		if err := CheckMirror(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for raw, want := range map[string]string{
		"":                                  "is empty",
		"ftp://mirror.example/simple":       "not an http or an https URL",
		"pkg-cache:8080/simple":             "not an http or an https URL",
		"http:///simple":                    "names no host",
		"http://bot:s3cr3t@mirror.example/": "holds a user or a password",
		"http://mirror.example/simple?x=1":  "query or a fragment",
		"http://mirror.example/a b":         "space or a control character",
	} {
		err := CheckMirror(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", raw, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%q: the error repeats the password", raw)
		}
	}
}
