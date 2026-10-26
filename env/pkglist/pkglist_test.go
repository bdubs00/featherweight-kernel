package pkglist

import (
	"strings"
	"testing"
)

// A requirement that starts with a dash, holds a control character, or is
// too long fails, and so does a list that is too long.
func TestCheck(t *testing.T) {
	for _, ok := range []string{"pyyaml", "pyyaml>=6", "ansible-lint[yaml]", strings.Repeat("a", MaxLength)} {
		if err := CheckRequirement(Python, ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, ok := range []string{"nodejs 22.*", "jq", strings.Repeat("a", MaxLength)} {
		if err := CheckRequirement(Tools, ok); err != nil {
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
		if err := CheckRequirement(Python, req); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", req, err, want)
		}
	}
	if err := Check(Python, []string{" pyyaml ", "six"}); err != nil {
		t.Fatal(err)
	}
	if err := Check(Python, []string{"six", " -x"}); err == nil || err.Error() != "item 1 starts with a dash" {
		t.Fatalf("err = %v", err)
	}
	if err := Check(Tools, make([]string, MaxItems+1)); err == nil || !strings.Contains(err.Error(), "holds 101 items; the limit is 100") {
		t.Fatalf("err = %v", err)
	}
	if err := Check(Tools, nil); err != nil {
		t.Fatal(err)
	}
	if err := CheckRequirement("pip", "six"); err == nil || !strings.Contains(err.Error(), `the list "pip"`) {
		t.Fatalf("an unknown list: err = %v", err)
	}
}

// A Python requirement is a name of PEP 508 with optional extras and
// version specifiers. A URL, a quote, an environment marker, a space
// inside the name, and any other text fail with a plain clause.
func TestCheckPython(t *testing.T) {
	for _, ok := range []string{
		"six", "PyYAML", "ruamel.yaml", "zope_interface", "a", "a1",
		"pyyaml>=6", "pyyaml >= 6", "pyyaml>=6,<7", "pyyaml >=6, <7", "pyyaml (>=6)", "pyyaml~=6.0",
		"pyyaml===6.0.1", "pyyaml!=5.*", "torch==2.4.0+cpu", "pkg==1.0.post1", "pkg==1!2.0",
		"ansible-lint[yaml]==24.9.2", "requests[socks, security]>=2", "requests [socks]",
	} {
		if err := CheckRequirement(Python, ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for req, want := range map[string]string{
		"bad name;rm":                    "holds a semicolon",
		"six; python_version > '3'":      "holds a quote",
		"six; sys_platform == linux":     "holds a semicolon",
		"requests @ https://e.org/r.whl": "names a URL",
		"git+https://e.org/r.git":        "names a URL",
		"bad name":                       "holds a space inside the name",
		`six"`:                           "holds a quote",
		"six`":                           "holds a quote",
		">=6":                            "is not a Python package",
		"six-":                           "is not a Python package",
		".six":                           "is not a Python package",
		"six>":                           "is not a Python package",
		"six>=6 7":                       "is not a Python package",
		"six[":                           "is not a Python package",
		"six[a,]":                        "is not a Python package",
		"six(>=6":                        "is not a Python package",
		"six>=6)":                        "is not a Python package",
		"six=6":                          "is not a Python package",
		"six>=6$":                        "is not a Python package",
		"six\\":                          "is not a Python package",
		"pyyaml\n]":                      "control character",
	} {
		if err := CheckRequirement(Python, req); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", req, err, want)
		}
	}
}

// A tool is a conda name with an optional version and an optional build.
// A channel, a bracket, a quote, and any other text fail.
func TestCheckTool(t *testing.T) {
	for _, ok := range []string{"jq", "nodejs>=22", "nodejs 22.*", "nodejs  >=22,<23", "openssl 3.* *_0", "python_abi", "nodejs==22.1.0=h123_0", "ripgrep 14.1|14.2"} {
		if err := CheckRequirement(Tools, ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for req, want := range map[string]string{
		"conda-forge::jq":     "names a channel",
		"jq[version='>=1.7']": "holds a quote",
		"jq[version=1.7]":     "is not a program",
		"_libgcc_mutex":       "is not a program",
		"jq 1.7 h1 extra":     "is not a program",
		"jq;rm":               "is not a program",
		"walk tool":           "is not a program",
		"jq 1.7 h$1":          "is not a program",
		"jq/1.7":              "is not a program",
		"jq\r":                "control character",
	} {
		if err := CheckRequirement(Tools, req); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", req, err, want)
		}
	}
}

// Every requirement that passes the check stays one basic string of TOML
// in a manifest: it holds no quote, no backslash, and no line break.
func TestCheckKeepsTOML(t *testing.T) {
	for _, manager := range []string{Python, Tools} {
		for _, req := range []string{"a\"b", "a'b", "a\\b", "a\nb", "a\rb", "a\u2028b"} {
			if CheckRequirement(manager, req) == nil {
				t.Errorf("%s %q passed", manager, req)
			}
		}
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
