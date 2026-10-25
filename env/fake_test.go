//go:build unix

package env

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// fakeUV is a stand-in for uv. It records each call and its environment
// under the log directory. sync makes a virtual environment from the lines
// "package <name> <version>" of uv.lock. A file named fail in the project
// fails sync with a URL that holds a password, and so does a lock file
// with the package nobuild. lock writes uv.lock from the dependencies of
// the manifest that fw writes, and fails for a package whose name starts
// with nosuch. add and remove change the manifest and the lock file.
const fakeUV = `#!/bin/sh
log=@LOG@
echo "uv $*" >> "$log/calls"
env | sort > "$log/uv-env"
case "$1" in
--version) echo "uv 0.0.0-fake"; exit 0 ;;
lock)
  project=""
  while [ $# -gt 0 ]; do [ "$1" = "--project" ] && project="$2"; shift; done
  if grep -q '^  "nosuch' "$project/pyproject.toml"; then
    echo "error: No solution found when resolving dependencies: nosuch was not found in the package registry" >&2
    exit 1
  fi
  echo "version = 1" > "$project/uv.lock"
  sed -n 's/^  "\([A-Za-z0-9._-]*\).*",$/package \1 1.0.0/p' "$project/pyproject.toml" >> "$project/uv.lock"
  ;;
sync)
  project=""
  while [ $# -gt 0 ]; do [ "$1" = "--project" ] && project="$2"; shift; done
  if [ -f "$project/fail" ] || grep -q '^package nobuild ' "$project/uv.lock"; then
    echo "error: Failed to fetch https://user:hunter2@index.example/simple/pyyaml" >&2
    exit 2
  fi
  mkdir -p "$UV_PROJECT_ENVIRONMENT/bin"
  printf '#!/bin/sh\necho Python 3.12.7\n' > "$UV_PROJECT_ENVIRONMENT/bin/python"
  chmod +x "$UV_PROJECT_ENVIRONMENT/bin/python"
  printf 'home = /usr/bin\nversion_info = 3.12.7\n' > "$UV_PROJECT_ENVIRONMENT/pyvenv.cfg"
  out="["; sep=""
  while read -r kind name version; do
    [ "$kind" = package ] || continue
    out="$out$sep{\"name\":\"$name\",\"version\":\"$version\"}"; sep=","
  done < "$project/uv.lock"
  echo "$out]" > "$UV_PROJECT_ENVIRONMENT/packages.json"
  ;;
pip)
  while [ $# -gt 0 ]; do [ "$1" = "--python" ] && venv="$2"; shift; done
  cat "$venv/packages.json"
  ;;
add|remove)
  cmd="$1"; project=""
  while [ $# -gt 0 ]; do
    case "$1" in --project) project="$2"; shift ;; --) shift; spec="$1" ;; esac
    shift
  done
  name=$(echo "$spec" | sed 's/[<>=!~].*//')
  if [ "$cmd" = add ]; then
    echo "# dependency $spec" >> "$project/pyproject.toml"
    echo "package $name 1.0.0" >> "$project/uv.lock"
  else
    grep -v "package $name " "$project/uv.lock" > "$project/uv.lock.new"; mv "$project/uv.lock.new" "$project/uv.lock"
  fi
  ;;
*) echo "fake uv: unknown command $1" >&2; exit 64 ;;
esac
`

// fakePixi is a stand-in for pixi. install makes an environment with a
// program jq under the directory that PIXI_HOME/config.toml names, in a
// directory named after the workspace. lock writes pixi.lock next to the
// manifest, and fails for a package whose name starts with nosuch.
const fakePixi = `#!/bin/sh
log=@LOG@
echo "pixi $*" >> "$log/calls"
env | sort > "$log/pixi-env"
manifest=""; prev=""
for a in "$@"; do [ "$prev" = "--manifest-path" ] && manifest="$a"; prev="$a"; done
name=example
[ -n "$manifest" ] && [ -f "$manifest" ] && name=$(sed -n 's/^name = "\(.*\)"$/\1/p' "$manifest" | head -n 1)
envs=$(sed -n 's/^detached-environments = "\(.*\)"$/\1/p' "$PIXI_HOME/config.toml")
prefix="$envs/$name-123/envs/default"
case "$1" in
--version) echo "pixi 0.0.0-fake" ;;
lock)
  if grep -q '^"nosuch' "$manifest"; then
    echo "Error: cannot solve the request because of: No candidates were found for nosuch" >&2
    exit 1
  fi
  grep '^"' "$manifest" > "$(dirname "$manifest")/pixi.lock"
  ;;
install)
  mkdir -p "$prefix/bin"
  printf '#!/bin/sh\necho jq-1.7.1\n' > "$prefix/bin/jq"
  chmod +x "$prefix/bin/jq"
  ;;
info) echo "{\"environments_info\":[{\"name\":\"default\",\"prefix\":\"$prefix\"}]}" ;;
list) echo '[{"name":"jq","version":"1.7.1","is_explicit":true},{"name":"libgcc","version":"14.2.0","is_explicit":false}]' ;;
add|remove)
  cmd="$1"
  while [ $# -gt 0 ]; do
    case "$1" in --manifest-path) manifest="$2"; shift ;; --) shift; spec="$1" ;; esac
    shift
  done
  echo "# $cmd $spec" >> "$manifest"
  echo "# $cmd $spec" >> "$(dirname "$manifest")/pixi.lock"
  ;;
*) echo "fake pixi: unknown command $1" >&2; exit 64 ;;
esac
`

// fakePython is a stand-in for the python3 of the image.
const fakePython = `#!/bin/sh
echo "python3 $*" >> @LOG@/calls
[ "$1" = "--version" ] && echo "Python 3.12.7"
exit 0
`

// fixture is a workspace with a block environment, fake managers first on
// PATH, and an agent with an in-memory bus.
type fixture struct {
	t    *testing.T
	root string
	logs string
	cfg  *fw.Config
	s    *spec
	a    *fw.Agent
	mu   sync.Mutex
	evs  []fw.Event
}

// newFixture writes the manifests and the block, and builds the agent.
// block is the YAML of the block without the key.
func newFixture(t *testing.T, block string) *fixture {
	t.Helper()
	if isDir(volume) {
		// The volume of ax decides the defaults, so a test machine with
		// /workspace would install there.
		t.Skip("this machine has " + volume)
	}
	base := t.TempDir()
	f := &fixture{t: t, root: filepath.Join(base, "ws"), logs: filepath.Join(base, "logs")}
	bin := filepath.Join(base, "bin")
	for _, d := range []string{f.root, f.logs, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, text := range map[string]string{"uv": fakeUV, "pixi": fakePixi, "python3": fakePython} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(strings.ReplaceAll(text, "@LOG@", f.logs)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	f.write("pyproject.toml", "[project]\nname = \"example\"\ndependencies = [\"pyyaml>=6\"]\n")
	f.write("uv.lock", "version = 1\npackage pyyaml 6.0.2\n")
	f.write("pixi.toml", "[workspace]\nname = \"example\"\n\n[dependencies]\njq = \"*\"\n")
	f.write("pixi.lock", "version: 6\nurl: https://conda.anaconda.org/conda-forge/linux-64/jq-1.7.1.conda\n")
	cfg := fw.DefaultConfig()
	cfg.Home = filepath.Join(base, "ws", ".fw")
	cfg.Tools.Files.Root = f.root
	cfg.Tools.User = ""
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{}
	if len(doc.Content) > 0 {
		cfg.Extensions[Block] = *doc.Content[0]
	}
	f.cfg = cfg
	// A block that does not load leaves s nil, for the tests of the checks.
	f.s, _ = load(cfg)
	return f
}

// reblock gives the fixture another block in the same workspace, as a
// new revision of a pool does, and loads it.
func (f *fixture) reblock(block string) {
	f.t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
		f.t.Fatal(err)
	}
	f.cfg.Extensions[Block] = *doc.Content[0]
	s, err := load(f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.s = s
}

// agent builds the agent of the fixture once and records the events of
// the environment from its bus.
func (f *fixture) agent() *fw.Agent {
	f.t.Helper()
	if f.a != nil {
		return f.a
	}
	bus := fw.NewMemBus()
	noModel := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return nil, errors.New("env test: no model")
	}}
	a, err := fw.New(fw.WithConfig(f.cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(noModel),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	stop, err := bus.Subscribe(context.Background(), Subject, func(e fw.Event) error {
		f.mu.Lock()
		f.evs = append(f.evs, e)
		f.mu.Unlock()
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(stop)
	f.a = a
	return a
}

// runCtx is the context of a tool call of a run of the agent.
func (f *fixture) runCtx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	f.t.Cleanup(cancel)
	ctx = fw.WithAgent(ctx, f.agent())
	return fw.WithRunInfo(ctx, fw.RunInfo{RunID: "run-1", Agent: "worker", Dir: f.root})
}

func (f *fixture) write(name, text string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, name), []byte(text), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(path string) string {
	f.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// calls returns the calls of the fake managers.
func (f *fixture) calls() []string {
	b, _ := os.ReadFile(filepath.Join(f.logs, "calls"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// count returns how many calls start with prefix.
func (f *fixture) count(prefix string) int {
	n := 0
	for _, c := range f.calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// events returns the events of the environment, waiting briefly for n.
func (f *fixture) events(n int) []fw.Event {
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		evs := append([]fw.Event(nil), f.evs...)
		f.mu.Unlock()
		if len(evs) >= n || time.Now().After(deadline) {
			return evs
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settled returns the events after a short pause, to check that no
// further event came.
func (f *fixture) settled() []fw.Event {
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fw.Event(nil), f.evs...)
}

func decode[T any](t *testing.T, e fw.Event) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(e.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// envValue returns the value of the last entry of name.
func envValue(env []string, name string) string {
	v := ""
	for _, kv := range env {
		if n, val, _ := strings.Cut(kv, "="); n == name {
			v = val
		}
	}
	return v
}
