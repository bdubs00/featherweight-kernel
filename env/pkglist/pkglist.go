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

// CheckRequirement checks one requirement, such as "pyyaml>=6" or
// "nodejs>=22". It refuses an empty requirement, a requirement that
// starts with a dash, which a manager could read as an option, a control
// character, and a requirement longer than MaxLength. The caller trims
// the spaces first. The error does not repeat the requirement.
func CheckRequirement(req string) error {
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
	return nil
}

// Check checks each requirement of a list, after it trims the spaces,
// and the length of the list. The error names the index of the bad item.
func Check(list []string) error {
	if len(list) > MaxItems {
		return fmt.Errorf("holds %d items; the limit is %d", len(list), MaxItems)
	}
	for i, req := range list {
		if err := CheckRequirement(strings.TrimSpace(req)); err != nil {
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
// Every tool process can read the URL in its environment, so a URL with a
// password would give the password to the model. The error does not
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
