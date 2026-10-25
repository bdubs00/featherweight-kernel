package fw

import (
	"fmt"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// ToolUserNone is the value of tools.user that runs the processes of tools
// as the user of fw itself.
const ToolUserNone = "none"

// ToolUser is the user and the group that run the processes of tools, such
// as the shell of bash and a stdio MCP server (tools.user, section 7.12).
// Such a process runs as UID and GID with no other group, so it cannot read
// the home directory of fw, its memory, or its entries under /proc.
type ToolUser struct {
	Spec string // the value of tools.user
	Name string // the name of the user in /etc/passwd, or "" for a uid:gid without an entry
	UID  uint32
	GID  uint32
	Home string // the home directory in /etc/passwd, or ""
}

// String names the user for a log line, such as "nonroot (65532:65532)".
func (u *ToolUser) String() string {
	if u == nil {
		return ToolUserNone
	}
	name := u.Name
	if name == "" {
		name = "uid " + strconv.FormatUint(uint64(u.UID), 10)
	}
	return fmt.Sprintf("%s (%d:%d)", name, u.UID, u.GID)
}

// LookupToolUser resolves a value of tools.user: a user name or a uid, with
// an optional ":group" or ":gid", such as "nonroot", "65532", or
// "65532:65532". An empty value and "none" give nil, and the tools run as
// the user of fw. A name, a bare uid, and a group name must exist in
// /etc/passwd and /etc/group; a uid:gid with two numbers needs no entry.
// The user and the group must not be root. Every error says what to set,
// because a user that does not exist must stop the start: a silent
// fallback runs the tools as fw.
func LookupToolUser(spec string) (*ToolUser, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == ToolUserNone {
		return nil, nil
	}
	if !toolUserSupported {
		return nil, fmt.Errorf("fw: tools.user %q needs Linux or another Unix system; set tools.user: none", spec)
	}
	name, group, hasGroup := strings.Cut(spec, ":")
	if name == "" || (hasGroup && group == "") {
		return nil, fmt.Errorf("fw: tools.user %q is not a user, a uid, user:group, or uid:gid", spec)
	}
	u := &ToolUser{Spec: spec}
	var entry *user.User
	if uid, err := strconv.ParseUint(name, 10, 32); err == nil {
		u.UID = uint32(uid)
		entry, err = user.LookupId(name)
		if err != nil {
			if _, numeric := parseID(group); !hasGroup || !numeric {
				return nil, fmt.Errorf("fw: tools.user names uid %s, which /etc/passwd does not list: create the user in the image, set tools.user to uid:gid, or set tools.user: none: %w", name, err)
			}
			entry = nil
		}
	} else {
		entry, err = user.Lookup(name)
		if err != nil {
			return nil, fmt.Errorf("fw: tools.user names the user %q, which does not exist: create the user in the image, set tools.user to a uid:gid, or set tools.user: none: %w", name, err)
		}
		id, ok := parseID(entry.Uid)
		if !ok {
			return nil, fmt.Errorf("fw: tools.user: the user %q has the uid %q, which is not a number", name, entry.Uid)
		}
		u.UID = id
	}
	if entry != nil {
		u.Name, u.Home = entry.Username, entry.HomeDir
	}
	switch {
	case hasGroup:
		if id, ok := parseID(group); ok {
			u.GID = id
			break
		}
		g, err := user.LookupGroup(group)
		if err != nil {
			return nil, fmt.Errorf("fw: tools.user names the group %q, which does not exist: create the group in the image, or name a gid: %w", group, err)
		}
		id, ok := parseID(g.Gid)
		if !ok {
			return nil, fmt.Errorf("fw: tools.user: the group %q has the gid %q, which is not a number", group, g.Gid)
		}
		u.GID = id
	default:
		id, ok := parseID(entry.Gid)
		if !ok {
			return nil, fmt.Errorf("fw: tools.user: the user %q has the gid %q, which is not a number", name, entry.Gid)
		}
		u.GID = id
	}
	if u.UID == 0 || u.GID == 0 {
		return nil, fmt.Errorf("fw: tools.user %q is root (uid or gid 0), which keeps nothing from the tools; name an unprivileged user, or set tools.user: none to run the tools as the user of fw", spec)
	}
	return u, nil
}

// ToolUserOf resolves the tool user of cfg: LookupToolUser of tools.user,
// with tools.userHome as its home directory when that is set. Every part
// of the kernel that starts a process as the tool user resolves the user
// here, so each process gets the same HOME. A tools.userHome that is not
// an absolute path is an error.
func ToolUserOf(cfg *Config) (*ToolUser, error) {
	if cfg == nil {
		return nil, nil
	}
	u, err := LookupToolUser(cfg.Tools.User)
	if err != nil || u == nil {
		return u, err
	}
	if h := strings.TrimSpace(cfg.Tools.UserHome); h != "" {
		if !filepath.IsAbs(h) {
			return nil, fmt.Errorf("fw: tools.userHome %q is not an absolute path", h)
		}
		u.Home = filepath.Clean(h)
	}
	return u, nil
}

// parseID reads a uid or a gid.
func parseID(s string) (uint32, bool) {
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err == nil
}

// ToolTreeMarker returns the file under the home directory that records
// that the tool user owns the workspace, so a later start skips the walk
// over every file (section 7.12). The ax boot removes it after it prepared
// a workspace again, because the new files belong to fw.
func ToolTreeMarker(home string) string { return filepath.Join(home, "tooluser") }
