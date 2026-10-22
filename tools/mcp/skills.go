package mcp

// This file reads the skills of a server (docs/ARCHITECTURE.md section
// 7.3, Skills of a server): the extension io.modelcontextprotocol/skills
// lists them with skills/list, and the files of a skill are resources
// that resources/read returns. A server behind a gateway that refuses the
// method gets the SKILL.md resources that the operator pins. The kernel
// reads a file at the moment of use and checks its size, its digest, and
// its front matter against the listing, or against its own first read.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// SkillsExtension is the name of the MCP extension of skills, which a
// server declares in its capabilities.
const SkillsExtension = "io.modelcontextprotocol/skills"

// Methods of the skills extension.
const (
	methodSkillsList = "skills/list"
	skillsListPages  = 100 // the most pages of one listing
)

// maxSkillRead bounds one file of a skill that the kernel reads.
const maxSkillRead = 4 << 20

// skillsRetry is how long a failed listing waits before the next try, so
// a server that is down does not slow every model call.
const skillsRetry = 30 * time.Second

// skillsListParams is the request of skills/list.
type skillsListParams struct {
	sdk.ParamsBase
	Cursor string `json:"cursor,omitempty"`
}

// skillsListResult is the answer of skills/list.
type skillsListResult struct {
	sdk.ResultBase
	Skills     []skillEntry `json:"skills"`
	NextCursor string       `json:"nextCursor,omitempty"`
	TTLMs      int64        `json:"ttlMs,omitempty"`
	CacheScope string       `json:"cacheScope,omitempty"`
}

// skillEntry is one skill of skills/list. Resources is an array of
// skillResource, or the string "dynamic" for a skill whose files the
// server lists on demand.
type skillEntry struct {
	URI         string          `json:"uri"`
	Frontmatter map[string]any  `json:"frontmatter"`
	Resources   json.RawMessage `json:"resources,omitempty"`
}

// skillResource is one file of a skill with its digest and its size.
type skillResource struct {
	URI    string `json:"uri"`
	Digest string `json:"digest,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// mcpSkill is one skill of the server as the kernel keeps it. uri names
// SKILL.md, and base is the URI that the files of the skill start with.
// digest and size are those of SKILL.md from the listing, or from the
// first read of a pinned or dynamic skill. files holds the listed
// supporting files by their path relative to the skill, and is nil for a
// skill whose files the server lists on demand. seen holds the digest of
// the first read of each file that the listing does not name.
type mcpSkill struct {
	name        string
	description string
	uri         string
	base        string
	digest      string
	size        int64
	files       map[string]skillResource
	pinned      bool
	seen        map[string]string
}

// skillState is the skills of a server and how the kernel got them.
type skillState struct {
	list    []*mcpSkill
	listed  bool      // list holds an answer of the server
	stale   bool      // the server reported a change of its resources
	expires time.Time // the ttl of the listing, zero for none
	retryAt time.Time // the next try after a failed listing
	refused bool      // the server declared the extension and refused skills/list
	status  string    // what fw doctor says
}

// SourceSkills implements fw.SkillSource. It lists the skills on first
// use, after the server reported a change of its resources, and after the
// ttl of the listing. A server that does not answer keeps its last list,
// and the log says so. A server that acts for the person of a run and a
// server with skills off have no skills.
func (s *Server) SourceSkills(ctx context.Context) []fw.Skill {
	if s.personal || s.cfg.Skills.Off() {
		return nil
	}
	s.mu.Lock()
	st := s.skills
	need := (!st.listed || st.stale || (!st.expires.IsZero() && s.now().After(st.expires))) && !s.now().Before(st.retryAt) && !s.downLocked()
	s.mu.Unlock()
	if need {
		s.refreshSkills(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fw.Skill, 0, len(s.skills.list))
	for _, m := range s.skills.list {
		out = append(out, fw.Skill{
			Name:        s.cfg.Name + "/" + m.name,
			Description: m.description,
			Origin:      fw.SkillOriginMCP,
			Server:      s.cfg.Name,
			URI:         m.uri,
		}.WithSkillSource(s))
	}
	return out
}

// SkillsStatus says how the server gives its skills, for fw doctor: how
// many it lists, and whether it answers skills/list or needs a pinned
// list.
func (s *Server) SkillsStatus() string {
	switch {
	case s.personal:
		return "acts for the person of a run, so the kernel reads no skill of it"
	case s.cfg.Skills.Off():
		return "skills off"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.skills.status == "" {
		return "skills not listed yet"
	}
	return s.skills.status
}

// resourcesChanged handles notifications/resources/list_changed: the next
// SourceSkills lists the skills again.
func (s *Server) resourcesChanged(context.Context, *sdk.ResourceListChangedRequest) {
	s.mu.Lock()
	s.skills.stale = true
	s.mu.Unlock()
	s.log.Debug("mcp resource list changed")
}

// refreshSkills lists the skills of the server: with skills/list when the
// server declares the extension and the entry allows it, else from the
// pinned URIs. A failure keeps the last list and waits skillsRetry before
// the next try.
func (s *Server) refreshSkills(ctx context.Context) {
	s.skillsMu.Lock()
	defer s.skillsMu.Unlock()
	s.mu.Lock()
	st := s.skills
	fresh := st.listed && !st.stale && (st.expires.IsZero() || !s.now().After(st.expires))
	s.mu.Unlock()
	if fresh {
		return // another caller listed while this one waited
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	fail := func(err error) {
		s.mu.Lock()
		s.skills.retryAt = s.now().Add(skillsRetry)
		s.skills.status = "could not list skills: " + err.Error()
		n := len(s.skills.list)
		s.mu.Unlock()
		s.log.Warn("mcp skills: the server did not answer; the last list stays", "skills", n, "error", err)
	}
	cs, err := s.connect(ctx)
	if err != nil {
		fail(err)
		return
	}
	pins := s.cfg.Skills.Pinned()
	declared := declaresSkills(cs)
	s.mu.Lock()
	refused := s.skills.refused
	s.mu.Unlock()
	var list []*mcpSkill
	var ttl time.Duration
	status := ""
	listed := false
	if s.cfg.Skills.Auto() && declared && !refused {
		list, ttl, err = s.listSkills(ctx, cs)
		switch {
		case err == nil:
			listed = true
			status = fmt.Sprintf("%d skills with skills/list", len(list))
		case methodRefused(err):
			refused = true
			s.mu.Lock()
			s.skills.refused = true
			s.mu.Unlock()
			s.log.Warn("mcp skills: the server declares the skills extension and does not answer skills/list; set tools.mcp[].skills to the URIs of its SKILL.md resources", "error", err)
		case len(pins) == 0 || ctx.Err() != nil:
			fail(err)
			return
		default:
			// Another failure takes the pins for this listing and asks
			// again at the next one.
			s.log.Warn("mcp skills: skills/list failed; the pinned skills stand in", "error", err)
		}
	}
	if !listed {
		switch {
		case len(pins) > 0:
			list, err = s.pinSkills(ctx, cs, pins)
			if err != nil {
				fail(err)
				return
			}
			switch {
			case refused:
				status = fmt.Sprintf("%d pinned skills; the server does not answer skills/list", len(list))
			case s.cfg.Skills.Auto():
				status = fmt.Sprintf("%d pinned skills; the server declares no skills extension", len(list))
			default:
				status = fmt.Sprintf("%d pinned skills", len(list))
			}
		case refused:
			status = "0 skills; the server does not answer skills/list and needs a pinned list in tools.mcp[].skills"
		default:
			status = "0 skills; the server declares no skills extension"
		}
	}
	s.mu.Lock()
	s.skills.list, s.skills.listed, s.skills.stale = list, true, false
	s.skills.retryAt, s.skills.status = time.Time{}, status
	s.skills.expires = time.Time{}
	if ttl > 0 {
		s.skills.expires = s.now().Add(ttl)
	}
	s.mu.Unlock()
	s.log.Debug("mcp skills listed", "count", len(list), "status", status)
}

// checkSkills checks tools.mcp[].skills: auto, off alone, or URIs with a
// scheme, with auto among them or not.
func checkSkills(v fw.MCPSkills) error {
	for _, e := range v {
		e = strings.TrimSpace(e)
		switch {
		case e == fw.MCPSkillsAuto:
		case e == fw.MCPSkillsOff:
			if len(v) > 1 {
				return errors.New("skills: off stands alone")
			}
		default:
			if u, err := url.Parse(e); err != nil || u.Scheme == "" {
				return fmt.Errorf("skills: %q is neither auto, off, nor the URI of a SKILL.md resource", e)
			}
		}
	}
	return nil
}

// declaresSkills reports whether the server declared the skills extension
// in its capabilities.
func declaresSkills(cs *sdk.ClientSession) bool {
	r := cs.InitializeResult()
	if r == nil || r.Capabilities == nil {
		return false
	}
	_, ok := r.Capabilities.Extensions[SkillsExtension]
	return ok
}

// methodRefused reports whether an error says that the server does not
// answer the method: the JSON-RPC error -32601, or an HTTP 404.
func methodRefused(err error) bool {
	var werr *jsonrpc.Error
	if errors.As(err, &werr) {
		return werr.Code == jsonrpc.CodeMethodNotFound
	}
	msg := err.Error()
	return strings.Contains(msg, "Not Found") || strings.Contains(msg, "404")
}

// listSkills calls skills/list and follows every page. It returns the
// skills, each name once, and the ttl of the listing.
func (s *Server) listSkills(ctx context.Context, cs *sdk.ClientSession) ([]*mcpSkill, time.Duration, error) {
	var out []*mcpSkill
	seen := map[string]bool{}
	var ttl time.Duration
	cursor := ""
	for page := 0; page < skillsListPages; page++ {
		res, err := sdk.CallCustomMethod[*skillsListParams, *skillsListResult](ctx, cs, methodSkillsList, &skillsListParams{Cursor: cursor})
		if err != nil {
			return nil, 0, fmt.Errorf("mcp: server %s: %s: %w", s.cfg.Name, methodSkillsList, err)
		}
		for _, e := range res.Skills {
			m, err := listedSkill(e)
			if err != nil {
				s.log.Warn("mcp skills: a skill of the listing is left out", "uri", e.URI, "error", err)
				continue
			}
			if seen[m.name] {
				s.log.Warn("mcp skills: the server lists a name twice; the first stays", "skill", m.name)
				continue
			}
			seen[m.name] = true
			out = append(out, m)
		}
		if res.TTLMs > 0 {
			ttl = time.Duration(res.TTLMs) * time.Millisecond
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, ttl, nil
}

// listedSkill turns one entry of skills/list into a skill. The name comes
// from the front matter and must be a valid skill name. SKILL.md is the
// entry URI when it names the file, else the listed resource of that name,
// else the file under the entry URI.
func listedSkill(e skillEntry) (*mcpSkill, error) {
	name, _ := e.Frontmatter["name"].(string)
	if name == "" || fw.ValidSkillName(name) != name {
		return nil, fmt.Errorf("the front matter has no valid name: %q", name)
	}
	desc, _ := e.Frontmatter["description"].(string)
	desc = cutDescription(desc)
	var resources []skillResource
	dynamic := false
	raw := strings.TrimSpace(string(e.Resources))
	switch {
	case raw == "" || raw == "null":
	case raw == `"dynamic"`:
		dynamic = true
	default:
		if err := json.Unmarshal(e.Resources, &resources); err != nil {
			return nil, fmt.Errorf("the resources of the skill are neither a list nor \"dynamic\": %w", err)
		}
	}
	skillMD := e.URI
	if path.Base(skillMD) != fw.SkillFile {
		found := false
		for _, r := range resources {
			if path.Base(r.URI) == fw.SkillFile {
				skillMD, found = r.URI, true
				break
			}
		}
		if !found {
			skillMD = strings.TrimSuffix(e.URI, "/") + "/" + fw.SkillFile
		}
	}
	m := &mcpSkill{name: name, description: desc, uri: skillMD, base: strings.TrimSuffix(skillMD, "/"+fw.SkillFile)}
	if !dynamic {
		m.files = map[string]skillResource{}
	}
	for _, r := range resources {
		if r.URI == skillMD {
			m.digest, m.size = r.Digest, r.Size
			continue
		}
		rel, ok := strings.CutPrefix(r.URI, m.base+"/")
		if !ok {
			continue // a file outside the skill
		}
		if clean, err := fw.SkillFilePath(rel); err == nil {
			m.files[clean] = r
		}
	}
	return m, nil
}

// pinSkills reads each pinned SKILL.md, takes its front matter, and
// computes its digest, which later reads must match.
func (s *Server) pinSkills(ctx context.Context, cs *sdk.ClientSession, uris []string) ([]*mcpSkill, error) {
	var out []*mcpSkill
	seen := map[string]bool{}
	for _, uri := range uris {
		data, err := s.readResource(ctx, cs, uri)
		if err != nil {
			s.log.Warn("mcp skills: a pinned skill cannot be read", "uri", uri, "error", err)
			continue
		}
		name, desc := fw.SkillFrontMatter(string(data))
		base := strings.TrimSuffix(uri, "/"+fw.SkillFile)
		if name == "" {
			name = path.Base(base)
			if u, err := url.Parse(base); err == nil && u.Path != "" {
				name = path.Base(u.Path)
			}
		}
		if fw.ValidSkillName(name) != name || name == "" {
			s.log.Warn("mcp skills: a pinned skill has no valid name", "uri", uri, "name", name)
			continue
		}
		if seen[name] {
			s.log.Warn("mcp skills: two pinned skills have one name; the first stays", "skill", name)
			continue
		}
		seen[name] = true
		out = append(out, &mcpSkill{name: name, description: cutDescription(desc), uri: uri, base: base,
			digest: digestOf(data), size: int64(len(data)), pinned: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// ReadSkill implements fw.SkillSource. It reads SKILL.md of a skill when
// file is "", else a supporting file, with resources/read at the moment of
// use, and checks it: the size and the digest against the listing, or
// against the first read of a pinned skill or of a file that the listing
// does not name, and the name in the front matter of SKILL.md against the
// listing. On a mismatch it returns an error with the
// reason and nothing of the file.
func (s *Server) ReadSkill(ctx context.Context, sk fw.Skill, file string) (string, error) {
	m := s.skillNamed(sk.Name)
	if m == nil {
		return "", fmt.Errorf("mcp: server %s lists no skill %s", s.cfg.Name, sk.Name)
	}
	uri := m.uri
	want := skillResource{URI: m.uri, Digest: m.digest, Size: m.size}
	label := sk.Name + "/" + fw.SkillFile
	if file != "" {
		clean, err := fw.SkillFilePath(file)
		if err != nil {
			return "", fmt.Errorf("mcp: skill %s: %w", sk.Name, err)
		}
		label = sk.Name + "/" + clean
		if m.files != nil {
			r, ok := m.files[clean]
			if !ok {
				return "", fmt.Errorf("mcp: skill %s lists no file %s; its files are: %s", sk.Name, clean, strings.Join(s.SkillFiles(sk), ", "))
			}
			uri, want = r.URI, r
		} else {
			uri = m.base + "/" + clean
			s.mu.Lock()
			want = skillResource{URI: uri, Digest: m.seen[clean]}
			s.mu.Unlock()
		}
	}
	if want.Size > maxSkillRead {
		return "", fmt.Errorf("mcp: %s has %d bytes; the kernel reads at most %d", label, want.Size, maxSkillRead)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	cs, err := s.connect(ctx)
	if err != nil {
		return "", err
	}
	data, err := s.readResource(ctx, cs, uri)
	if err != nil {
		return "", err
	}
	if err := checkResource(label, data, want); err != nil {
		s.log.Warn("mcp skills: a file of a skill does not match", "skill", sk.Name, "uri", uri, "error", err)
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("mcp: %s is not text; the skill tool reads text files", label)
	}
	text := string(data)
	if file == "" {
		// The name of the front matter must be the name of the listing, or
		// of the first read of a pinned skill.
		if name, _ := fw.SkillFrontMatter(text); name != m.name && (!m.pinned || name != "") {
			err := fmt.Errorf("mcp: the front matter of %s names %q, and the listing says %q; the kernel does not use it", label, name, m.name)
			s.log.Warn("mcp skills: the front matter of a skill does not match", "skill", sk.Name, "error", err)
			return "", err
		}
	}
	if want.Digest == "" && file != "" {
		// The first read of a file that the listing does not name sets
		// the digest that later reads must match.
		s.mu.Lock()
		if m.seen == nil {
			m.seen = map[string]string{}
		}
		m.seen[path.Clean(file)] = digestOf(data)
		s.mu.Unlock()
	}
	return text, nil
}

// SkillFiles implements fw.SkillSource: the supporting files that the
// listing names, by their path relative to the skill, or nil for a skill
// whose files the server lists on demand and for a pinned skill.
func (s *Server) SkillFiles(sk fw.Skill) []string {
	m := s.skillNamed(sk.Name)
	if m == nil || m.files == nil {
		return nil
	}
	out := make([]string, 0, len(m.files))
	for rel := range m.files {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

// skillNamed finds a skill by its qualified name "<server>/<name>".
func (s *Server) skillNamed(qualified string) *mcpSkill {
	name, ok := strings.CutPrefix(qualified, s.cfg.Name+"/")
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.skills.list {
		if m.name == name {
			return m
		}
	}
	return nil
}

// readResource reads one resource with resources/read and returns the
// bytes of its first content: the text, else the blob.
func (s *Server) readResource(ctx context.Context, cs *sdk.ClientSession, uri string) ([]byte, error) {
	res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
	if err != nil {
		var werr *jsonrpc.Error
		if !errors.As(err, &werr) && ctx.Err() == nil {
			s.failed("resources/read", err)
		}
		return nil, fmt.Errorf("mcp: server %s: resources/read %s: %w", s.cfg.Name, uri, err)
	}
	s.succeeded()
	if len(res.Contents) == 0 || res.Contents[0] == nil {
		return nil, fmt.Errorf("mcp: server %s: resources/read %s returned no content", s.cfg.Name, uri)
	}
	c := res.Contents[0]
	data := []byte(c.Text)
	if len(c.Blob) > 0 {
		data = c.Blob
	}
	if len(data) > maxSkillRead {
		return nil, fmt.Errorf("mcp: server %s: %s has more than %d bytes", s.cfg.Name, uri, maxSkillRead)
	}
	return data, nil
}

// checkResource compares the bytes of a file with its size and digest.
func checkResource(label string, data []byte, want skillResource) error {
	if want.Size > 0 && int64(len(data)) != want.Size {
		return fmt.Errorf("mcp: %s has %d bytes, and the listing says %d; the kernel does not use it", label, len(data), want.Size)
	}
	if want.Digest != "" {
		if got := digestOf(data); !strings.EqualFold(got, want.Digest) {
			return fmt.Errorf("mcp: %s has the digest %s, and the listing says %s; the kernel does not use it", label, got, want.Digest)
		}
	}
	return nil
}

// digestOf returns "sha256:" and the hex SHA-256 of data.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// cutDescription cuts a description as the kernel cuts the description
// of a skill of the directories.
func cutDescription(d string) string { return fw.CutSkillDescription(strings.TrimSpace(d)) }
