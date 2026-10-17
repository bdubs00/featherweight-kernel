package ax

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// StockMarkerDir is where the stock ax runner writes its markers. It is not
// on the durable volume, but Prepare honors a marker that it finds there.
const StockMarkerDir = "/ax"

const (
	markerPrefix = "initialized"
	dirPerm      = 0o755
	filePerm     = 0o644
	stateDirPerm = 0o700

	cloneAttempts   = 3
	cloneRetryDelay = 2 * time.Second
)

// PrepareResult reports what Prepare did for one binding.
type PrepareResult struct {
	// Name is the name of the binding, empty for the default binding.
	Name string
	// Path is the absolute directory of the binding.
	Path string
	// FirstRun is true when no marker existed and the setup ran.
	FirstRun bool
	// Cloned lists the repositories cloned in this call. URLs carry no
	// credentials.
	Cloned []string
	// Marked is true when Prepare wrote the marker. A failed clone leaves it
	// false so that the next start tries again.
	Marked bool
	// Goal is the goal to bootstrap. It is set only on the first run and only
	// when the binding has a goal.
	Goal string
}

// MarkerName returns the marker file name for a workspace at path. The name
// matches the stock ax runner: "initialized-" plus the cleaned path with the
// slashes replaced by "-". For example, "/workspace" gives
// "initialized-workspace" and "/" gives "initialized-root".
func MarkerName(path string) string {
	if path == "" {
		path = DefaultWorkspacePath
	}
	clean := strings.Trim(filepath.Clean(path), "/")
	if clean == "" {
		clean = "root"
	}
	return markerPrefix + "-" + strings.ReplaceAll(clean, "/", "-")
}

// MarkerDir returns the directory where fw writes its markers: <home>/ax.
func MarkerDir(home string) string { return filepath.Join(home, "ax") }

// Prepare sets up each binding once, in order. It skips a binding whose
// marker exists under <home>/ax or under /ax. Otherwise it creates the path,
// writes spec.files, clones spec.git, creates spec.skills.path, and writes
// the marker under <home>/ax.
//
// Prepare does not run the goal. The caller runs Bootstrap for each result
// with a Goal. A failed clone is logged and leaves the marker unwritten, so
// the next start retries it, like the stock runner. Prepare returns an error
// only when a binding directory cannot be created; it still prepares the
// other bindings.
func Prepare(ctx context.Context, bindings []Binding, home string, log *slog.Logger) ([]PrepareResult, error) {
	if log == nil {
		log = slog.Default()
	}
	var errs []error
	out := make([]PrepareResult, 0, len(bindings))
	for _, b := range bindings {
		res, err := prepareOne(ctx, b, home, log)
		if err != nil {
			errs = append(errs, err)
		}
		out = append(out, res)
	}
	return out, errors.Join(errs...)
}

func prepareOne(ctx context.Context, b Binding, home string, log *slog.Logger) (PrepareResult, error) {
	target := b.Path
	if target == "" {
		target = DefaultWorkspacePath
	}
	if abs, err := filepath.Abs(target); err == nil {
		target = abs
	}
	res := PrepareResult{Name: b.Ref.Name, Path: target}
	log = log.With("workspace", b.Ref.Name, "path", target)

	if err := os.MkdirAll(target, dirPerm); err != nil {
		return res, fmt.Errorf("ax: create workspace %s: %w", target, err)
	}
	if ws := b.Workspace; ws != nil {
		// A workspace that an earlier version prepared can still hold a
		// token in .git/config, so this runs at every start.
		forgetAll(ws.Spec.Git, target, log)
	}
	name := MarkerName(target)
	ownMarker := filepath.Join(MarkerDir(home), name)
	for _, m := range []string{ownMarker, filepath.Join(StockMarkerDir, name)} {
		if _, err := os.Stat(m); err == nil {
			log.Info("ax: workspace already prepared; skipping setup", "marker", m)
			return res, nil
		}
	}

	log.Info("ax: preparing workspace")
	res.FirstRun = true
	gitOK := true
	if ws := b.Workspace; ws != nil {
		writeFiles(ws.Spec.Files, target, log)
		res.Cloned, gitOK = cloneRepos(ctx, ws.Spec.Git, target, log)
		makeSkillsDir(ws.Spec.Skills.Path, target, log)
	}
	if b.Ref.Goal != "" {
		res.Goal = b.Ref.Goal
	}
	if !gitOK {
		log.Warn("ax: workspace setup finished with git errors; marker not written so the next start retries")
		return res, nil
	}
	if err := writeMarker(ownMarker, b.Workspace); err != nil {
		log.Warn("ax: write workspace marker", "marker", ownMarker, "error", err)
		return res, nil
	}
	res.Marked = true
	log.Info("ax: workspace prepared", "marker", ownMarker, "cloned", len(res.Cloned))
	return res, nil
}

// writeFiles writes the inline files. A relative path is relative to the
// workspace. An absolute path is used as it is, as the stock runner does.
func writeFiles(files []File, target string, log *slog.Logger) {
	for _, f := range files {
		if f.Path == "" {
			continue
		}
		dest := f.Path
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(target, dest)
		}
		if err := os.MkdirAll(filepath.Dir(dest), dirPerm); err != nil {
			log.Warn("ax: create directory for workspace file", "file", dest, "error", err)
			continue
		}
		if err := os.WriteFile(dest, []byte(f.Content), filePerm); err != nil {
			log.Warn("ax: write workspace file", "file", dest, "error", err)
			continue
		}
		log.Info("ax: wrote workspace file", "file", dest)
	}
}

// makeSkillsDir creates spec.skills.path. A relative path is relative to the
// workspace.
func makeSkillsDir(p, target string, log *slog.Logger) {
	if p == "" {
		return
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(target, p)
	}
	if err := os.MkdirAll(p, dirPerm); err != nil {
		log.Warn("ax: create skills directory", "dir", p, "error", err)
		return
	}
	log.Info("ax: created skills directory", "dir", p)
}

// cloneRepos clones each repository. It returns the repositories that it
// cloned and whether no clone failed. A destination that already holds a
// .git directory is skipped and counts as a success.
func cloneRepos(ctx context.Context, repos []GitRepo, target string, log *slog.Logger) ([]string, bool) {
	var cloned []string
	ok := true
	for _, r := range repos {
		if r.Repo == "" {
			continue
		}
		safe := RedactURL(r.Repo)
		dest := CloneDir(r, target)
		rlog := log.With("repo", safe, "dir", dest)
		if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
			rlog.Info("ax: repository already present; skipping clone")
			continue
		}
		rlog.Info("ax: cloning repository", "branch", r.Branch, "depth", r.Depth)
		if err := cloneWithRetry(ctx, r, dest, rlog); err != nil {
			ok = false
			rlog.Warn("ax: clone failed; continuing setup", "error", redactErr(err, r.Repo))
			continue
		}
		forget(dest, rlog)
		cloned = append(cloned, safe)
	}
	return cloned, ok
}

// CloneDir returns where a repository is cloned, with the rules of the stock
// runner. An explicit dir wins: "." is the workspace itself, an absolute dir
// is used as it is, and a relative dir is under the workspace. Otherwise the
// directory is the repository name, or the last element of the URL when the
// name is empty, "repo", or "origin".
func CloneDir(r GitRepo, target string) string {
	switch {
	case r.Dir == ".":
		return target
	case r.Dir != "" && filepath.IsAbs(r.Dir):
		return r.Dir
	case r.Dir != "":
		return filepath.Join(target, r.Dir)
	}
	name := r.Name
	if name == "" || name == "repo" || name == "origin" {
		if derived := repoDirName(r.Repo); derived != "" {
			name = derived
		} else if name == "" {
			name = "repo"
		}
	}
	return filepath.Join(target, name)
}

func repoDirName(repoURL string) string {
	s := strings.TrimSuffix(strings.TrimSpace(repoURL), "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func cloneWithRetry(ctx context.Context, r GitRepo, dest string, log *slog.Logger) error {
	var err error
	for attempt := 1; attempt <= cloneAttempts; attempt++ {
		err = cloneOnce(ctx, r, dest)
		if err == nil || permanentCloneError(err) || attempt == cloneAttempts {
			return err
		}
		log.Warn("ax: clone failed; retrying", "attempt", attempt, "error", redactErr(err, r.Repo))
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(cloneRetryDelay):
		}
	}
	return err
}

func cloneOnce(ctx context.Context, r GitRepo, dest string) error {
	_, statErr := os.Stat(dest)
	existed := statErr == nil
	opts := &git.CloneOptions{URL: r.Repo}
	if r.Branch != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(r.Branch)
		opts.SingleBranch = true
	}
	if r.Depth > 0 {
		opts.Depth = r.Depth
		opts.SingleBranch = true
	}
	if _, err := git.PlainCloneContext(ctx, dest, false, opts); err != nil {
		// go-git removes what it created only when the destination was empty.
		// Remove a partial .git so that the next attempt does not skip it.
		if existed {
			_ = os.RemoveAll(filepath.Join(dest, ".git"))
		}
		return err
	}
	return nil
}

// forgetAll removes the credentials from the remote URLs of each
// repository of spec.git that is present.
func forgetAll(repos []GitRepo, target string, log *slog.Logger) {
	for _, r := range repos {
		if r.Repo == "" {
			continue
		}
		dest := CloneDir(r, target)
		if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
			forget(dest, log.With("repo", RedactURL(r.Repo), "dir", dest))
		}
	}
}

// forget removes the credentials from the remote URLs of the repository at
// dir. go-git stores the clone URL as the URL of origin, so a token in
// spec.git stays in .git/config, where a tool can read it. A later fetch or
// push needs a credential from somewhere else, such as the egress policy.
func forget(dir string, log *slog.Logger) {
	repo, err := git.PlainOpen(dir)
	if err != nil {
		log.Warn("ax: open repository to remove credentials", "error", err)
		return
	}
	cfg, err := repo.Config()
	if err != nil {
		log.Warn("ax: read repository configuration", "error", err)
		return
	}
	changed := false
	for _, rc := range cfg.Remotes {
		for i, u := range rc.URLs {
			if clean := withoutCredentials(u); clean != u {
				rc.URLs[i] = clean
				changed = true
			}
		}
	}
	if !changed {
		return
	}
	if err := repo.SetConfig(cfg); err != nil {
		log.Warn("ax: remove credentials from the repository configuration", "error", err)
		return
	}
	log.Info("ax: removed credentials from the remote URL")
}

// withoutCredentials returns a remote URL without its credentials: all the
// user information of an http or https URL, and the user information of any
// other URL that holds a password. An ssh user such as git stays.
func withoutCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPassword := u.User.Password(); !hasPassword && u.Scheme != "http" && u.Scheme != "https" {
		return raw
	}
	u.User = nil
	return u.String()
}

func permanentCloneError(err error) bool {
	for _, target := range []error{
		transport.ErrRepositoryNotFound,
		transport.ErrEmptyRemoteRepository,
		transport.ErrAuthenticationRequired,
		transport.ErrAuthorizationFailed,
		transport.ErrInvalidAuthMethod,
		plumbing.ErrReferenceNotFound,
		git.ErrRepositoryAlreadyExists,
		context.Canceled,
		context.DeadlineExceeded,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return errors.Is(err, git.NoMatchingRefSpecError{})
}

func writeMarker(p string, ws *Workspace) error {
	if err := os.MkdirAll(filepath.Dir(p), stateDirPerm); err != nil {
		return err
	}
	name := "unknown"
	if ws != nil && ws.Metadata.Name != "" {
		name = ws.Metadata.Name
	}
	content := fmt.Sprintf("workspace: %s\ninitialized_at: %s\n", name, time.Now().UTC().Format(time.RFC3339))
	return os.WriteFile(p, []byte(content), filePerm)
}

// RedactURL replaces the user information of a URL with "***", so that a
// repository URL with a token can go into a log line.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	s := u.String()
	if i := strings.Index(s, "://"); i >= 0 {
		return s[:i+3] + "***@" + s[i+3:]
	}
	return s
}

// redactErr returns the error text with any credentials of repoURL removed.
func redactErr(err error, repoURL string) string {
	msg := err.Error()
	u, perr := url.Parse(repoURL)
	if perr != nil || u.User == nil {
		return msg
	}
	msg = strings.ReplaceAll(msg, repoURL, RedactURL(repoURL))
	// A token sits in the password, or in the user name when no password is
	// given, as in https://TOKEN@host/repo.git.
	secret, ok := u.User.Password()
	if !ok || secret == "" {
		secret = u.User.Username()
	}
	if len(secret) >= 6 {
		msg = strings.ReplaceAll(msg, secret, "***")
	}
	return msg
}
