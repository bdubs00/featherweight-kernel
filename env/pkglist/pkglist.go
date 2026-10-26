// Package pkglist checks the package lists and the mirrors of the block
// environment of fw.yaml (docs/ARCHITECTURE.md section 18.2). It has no
// init function and registers nothing. The workbench can therefore import
// it to check a list before it writes the list into a pool, without the
// tool env, the prompt section, or the service of the package env.
package pkglist

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// The limits of a list. A longer list or a longer requirement stops the
// start of fw.
const (
	// MaxItems is the largest number of requirements in one list.
	MaxItems = 100
	// MaxLength is the largest number of bytes in one requirement.
	MaxLength = 200
)

// The managers of the lists: the keys under environment.packages and
// environment.mirrors.
const (
	// Python is the list of Python packages, which uv installs.
	Python = "python"
	// Tools is the list of conda-forge packages, which pixi installs.
	Tools = "tools"
)

// CheckRequirement checks one requirement of the list of the manager.
// For Python it must be a requirement of PEP 508 without a URL and
// without an environment marker: a name, optional extras, and optional
// version specifiers, such as "pyyaml", "pyyaml>=6", or
// "ansible-lint[yaml]==24.9.2". For tools it must be a conda name with an
// optional version and an optional build, such as "jq", "nodejs>=22",
// "nodejs 22.*", or "openssl 3.* *_0", without a channel. It refuses
// everything else, and an empty requirement, a requirement that starts
// with a dash, a control character, and a requirement longer than
// MaxLength. A manager can read a requirement with a dash as an option,
// and the kernel writes each requirement into a TOML manifest, so a quote
// or a line break never passes. The caller trims the spaces first. The
// error is a clause that follows the requirement, such as "holds a
// semicolon", and it does not repeat the requirement.
func CheckRequirement(manager, req string) error {
	switch {
	case req == "":
		return errors.New("is empty")
	case len(req) > MaxLength:
		return fmt.Errorf("is longer than %d characters", MaxLength)
	case strings.HasPrefix(req, "-"):
		return errors.New("starts with a dash")
	}
	for _, r := range req {
		if unicode.IsControl(r) {
			return errors.New("holds a control character")
		}
	}
	if strings.ContainsAny(req, "\"'`") {
		return errors.New("holds a quote, which a package name never holds")
	}
	switch manager {
	case Python:
		return checkPython(req)
	case Tools:
		return checkTool(req)
	}
	return fmt.Errorf("belongs to the list %q, which is not %s or %s", manager, Python, Tools)
}

// Clauses of a refusal.
const (
	notPython = "is not a Python package: write a name with optional extras and a version, such as pyyaml, pyyaml>=6, or ansible-lint[yaml]==24.9.2"
	notTool   = "is not a program: write a conda name with an optional version and build, such as jq, nodejs>=22, or nodejs 22.*"
)

// checkPython checks a requirement of PEP 508 without a URL and without
// an environment marker.
func checkPython(req string) error {
	switch {
	case strings.Contains(req, ";"):
		return errors.New("holds a semicolon; a condition after a semicolon is not available, so write the name and the version alone, such as pyyaml>=6")
	case strings.Contains(req, "@") || strings.Contains(req, "://"):
		return errors.New("names a URL; name a package of the index instead, such as pyyaml>=6")
	}
	i := nameEnd(req, 0)
	if i == 0 {
		return errors.New(notPython)
	}
	j := skipSpaces(req, i)
	if j > i && j < len(req) && isAlnumByte(req[j]) {
		return errors.New("holds a space inside the name")
	}
	i = j
	if i < len(req) && req[i] == '[' {
		i = skipSpaces(req, i+1)
		for i < len(req) && req[i] != ']' {
			end := nameEnd(req, i)
			if end == i {
				return errors.New(notPython)
			}
			i = skipSpaces(req, end)
			if i < len(req) && req[i] == ',' {
				i = skipSpaces(req, i+1)
				if i < len(req) && req[i] == ']' {
					// A comma needs a name after it.
					return errors.New(notPython)
				}
				continue
			}
			break
		}
		if i >= len(req) || req[i] != ']' {
			return errors.New(notPython)
		}
		i = skipSpaces(req, i+1)
	}
	if i == len(req) {
		return nil
	}
	paren := req[i] == '('
	if paren {
		i = skipSpaces(req, i+1)
	}
	for {
		op := versionOp(req[i:])
		if op == 0 {
			return errors.New(notPython)
		}
		i = skipSpaces(req, i+op)
		start := i
		for i < len(req) && (isAlnumByte(req[i]) || strings.IndexByte("_.*+!-", req[i]) >= 0) {
			i++
		}
		if i == start {
			return errors.New(notPython)
		}
		i = skipSpaces(req, i)
		if i < len(req) && req[i] == ',' {
			i = skipSpaces(req, i+1)
			continue
		}
		break
	}
	if paren {
		if i >= len(req) || req[i] != ')' {
			return errors.New(notPython)
		}
		i = skipSpaces(req, i+1)
	}
	if i != len(req) {
		return errors.New(notPython)
	}
	return nil
}

// checkTool checks a conda name with an optional version and an optional
// build. A version can follow the name at once, as in "nodejs>=22", or
// after a space, as in "nodejs 22.*".
func checkTool(req string) error {
	if strings.Contains(req, "::") {
		return errors.New("names a channel; a channel is not available, so name a package of conda-forge, such as jq")
	}
	end := 0
	for end < len(req) && (isAlnumByte(req[end]) || strings.IndexByte("-_.", req[end]) >= 0) {
		end++
	}
	if end == 0 || !isAlnumByte(req[0]) {
		return errors.New(notTool)
	}
	rest := req[end:]
	if rest == "" {
		return nil
	}
	if !strings.ContainsRune("<>=!~", rune(rest[0])) && rest[0] != ' ' {
		return errors.New(notTool)
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 || len(fields) > 2 {
		return errors.New(notTool)
	}
	if c := fields[0][0]; !(c >= '0' && c <= '9') && strings.IndexByte("<>=!~*", c) < 0 {
		// A version starts with a digit, a star, or an operator.
		return errors.New(notTool)
	}
	for i, f := range fields {
		allowed := "<>=!~*.,|_+-"
		if i == 1 {
			allowed = "*._+-"
		}
		for j := 0; j < len(f); j++ {
			if !isAlnumByte(f[j]) && strings.IndexByte(allowed, f[j]) < 0 {
				return errors.New(notTool)
			}
		}
	}
	return nil
}

// nameEnd returns the end of a name of PEP 508 that starts at i, or i when
// none starts there. A name starts and ends with a letter or a digit and
// holds letters, digits, dots, dashes, and underscores. A name that ends
// with a dot, a dash, or an underscore is no name.
func nameEnd(s string, i int) int {
	if i >= len(s) || !isAlnumByte(s[i]) {
		return i
	}
	j := i
	for j < len(s) && isNameByte(s[j]) {
		j++
	}
	if !isAlnumByte(s[j-1]) {
		return i
	}
	return j
}

// versionOp returns the length of the version operator of PEP 440 that
// starts s, or 0.
func versionOp(s string) int {
	for _, op := range []string{"===", "~=", "==", "!=", "<=", ">=", "<", ">"} {
		if strings.HasPrefix(s, op) {
			return len(op)
		}
	}
	return 0
}

// skipSpaces returns the first index from i that is not a space.
func skipSpaces(s string, i int) int {
	for i < len(s) && s[i] == ' ' {
		i++
	}
	return i
}

func isAlnumByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// Check checks each requirement of the list of the manager, after it
// trims the spaces, and the length of the list. The error names the index
// of the bad item.
func Check(manager string, list []string) error {
	if len(list) > MaxItems {
		return fmt.Errorf("holds %d items; the limit is %d", len(list), MaxItems)
	}
	for i, req := range list {
		if err := CheckRequirement(manager, strings.TrimSpace(req)); err != nil {
			return fmt.Errorf("item %d %w", i, err)
		}
	}
	return nil
}

// Name returns the name of the package of a requirement, in the form
// that compares two requirements of one manager. For Python it is the
// normal form of PEP 503: lower case, and each run of "-", "_", and "."
// becomes one "-". For tools it is the conda name in lower case, after a
// channel prefix such as "conda-forge::". It returns "" when the
// requirement names no package.
func Name(manager, req string) string {
	req = strings.TrimSpace(req)
	if manager == Tools {
		if i := strings.LastIndex(req, "::"); i >= 0 {
			req = req[i+2:]
		}
	}
	end := 0
	for end < len(req) && isNameByte(req[end]) {
		end++
	}
	name := strings.ToLower(req[:end])
	if manager != Python {
		return name
	}
	var b strings.Builder
	sep := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '-' || c == '_' || c == '.' {
			sep = true
			continue
		}
		if sep && b.Len() > 0 {
			b.WriteByte('-')
		}
		sep = false
		b.WriteByte(c)
	}
	return b.String()
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
}

// CheckMirror checks the URL of a mirror. It must be an http or https URL
// with a host, without a user or a password, a query, or a fragment.
// Every tool process can read the URL in its environment, so the model
// can read a password of the URL too. The error does not
// repeat the URL.
func CheckMirror(raw string) error {
	if raw == "" {
		return errors.New("is empty")
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return errors.New("holds a space or a control character")
		}
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return errors.New("is not a URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("is not an http or an https URL")
	case u.Host == "" || u.Hostname() == "":
		return errors.New("names no host")
	case u.User != nil:
		return errors.New("holds a user or a password; every tool process can read the URL, so a mirror must not need them")
	case u.RawQuery != "" || u.Fragment != "" || u.ForceQuery:
		return errors.New("holds a query or a fragment")
	}
	return nil
}
