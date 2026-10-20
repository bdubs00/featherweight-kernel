package env

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The versions of uv and pixi that the Dockerfile installs, with the
// sha256 of each release archive (the musl builds, which are static). A
// change of a version needs the new checksums too.
const (
	uvVersion   = "0.13.1"
	pixiVersion = "0.82.0"
)

var checksums = map[string]map[string]string{
	"x86_64": {
		"uv":   "6c86040a8f045ab9adc9e3ec4cdafa03fd00e9a988ed2bf28971c7fc2d0debd0",
		"pixi": "d305406a7630aab6484419833f9204b2fe9ea40632da8bb536496f123ca86e7f",
	},
	"aarch64": {
		"uv":   "345a8308e4d1835cdfaadf692836fd3137a6d45864773f05ad7bf59442b1ba16",
		"pixi": "b18dadba196c0c77781533184ec3e7757422c0ae9bed0047ff7ca1c688c0f62b",
	},
}

// buildCache is the cache of the managers while the image builds. It is a
// cache mount of BuildKit, so it stays out of the image.
const buildCache = "/root/.cache/fw-env"

// safePath matches a path that the Dockerfile can name without quoting.
var safePath = regexp.MustCompile(`^[A-Za-z0-9._/@+-]+$`)

// writeDockerfile writes the Dockerfile of the block of cfg FROM the
// image from, to w (fw env dockerfile).
func writeDockerfile(cfg *fw.Config, from string, w io.Writer) error {
	s, err := load(cfg)
	if err != nil {
		return err
	}
	if s == nil {
		return errors.New("env: the configuration has no block environment")
	}
	text, err := s.dockerfile(from)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, text)
	return err
}

// dockerfile returns the Dockerfile text for placement image. The build
// context is the root of the workspace. A block with placement image keeps
// its dir. A durable block gives the same packages under /opt/environment,
// and the comment at the top says which block uses the image.
func (s *spec) dockerfile(from string) (string, error) {
	from = strings.TrimSpace(from)
	if from == "" || strings.ContainsAny(from, " \t\n") {
		return "", fmt.Errorf("env: the base image %q is not an image reference", from)
	}
	img := *s
	if img.placement != PlacementImage {
		img.placement, img.dir = PlacementImage, imageDir
	}
	if !safePath.MatchString(img.dir) {
		return "", fmt.Errorf("env: environment.dir %s holds characters that a Dockerfile must quote", img.dir)
	}
	digest := s.digest()
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	p("# syntax=docker/dockerfile:1\n")
	p("# Written by fw env dockerfile from the block environment of fw.yaml\n")
	p("# (docs/ARCHITECTURE.md section 18). Build it with the root of the workspace\n")
	p("# as the context, for example:\n")
	p("#   fw env dockerfile --from %s > Dockerfile.environment\n", from)
	p("#   docker build -f Dockerfile.environment -t <image> .\n")
	p("# A Task of the image uses this block:\n")
	p("#   environment:\n#     placement: image\n#     dir: %s\n", img.dir)
	p("# The digest of the manifests, the lock files, and the prepare commands:\n# %s\n", digest)
	p("FROM %s\n\n", from)
	p("ARG TARGETARCH\n")
	p("RUN set -eu; \\\n")
	p("    case \"${TARGETARCH:-amd64}\" in \\\n")
	for _, a := range []struct{ docker, rust string }{{"amd64", "x86_64"}, {"arm64", "aarch64"}} {
		p("      %s) arch=%s; uv_sha=%s; pixi_sha=%s ;; \\\n", a.docker, a.rust, checksums[a.rust]["uv"], checksums[a.rust]["pixi"])
	}
	p("      *) echo \"fw env: no uv and pixi for $TARGETARCH\" >&2; exit 1 ;; \\\n")
	p("    esac; \\\n")
	p("    curl -fsSL -o /tmp/uv.tar.gz \"https://github.com/astral-sh/uv/releases/download/%s/uv-${arch}-unknown-linux-musl.tar.gz\"; \\\n", uvVersion)
	p("    echo \"${uv_sha}  /tmp/uv.tar.gz\" | sha256sum -c -; \\\n")
	p("    tar -xzf /tmp/uv.tar.gz -C /usr/local/bin --strip-components=1 \"uv-${arch}-unknown-linux-musl/uv\" \"uv-${arch}-unknown-linux-musl/uvx\"; \\\n")
	p("    curl -fsSL -o /tmp/pixi.tar.gz \"https://github.com/prefix-dev/pixi/releases/download/v%s/pixi-${arch}-unknown-linux-musl.tar.gz\"; \\\n", pixiVersion)
	p("    echo \"${pixi_sha}  /tmp/pixi.tar.gz\" | sha256sum -c -; \\\n")
	p("    tar -xzf /tmp/pixi.tar.gz -C /usr/local/bin pixi; \\\n")
	p("    rm /tmp/uv.tar.gz /tmp/pixi.tar.gz\n\n")

	// The variables of the environment, as every tool process gets them.
	// The caches stay out: while the image builds they are in the cache
	// mount, and at run time the kernel gives a tool process the cache of
	// the configuration.
	var vars []string
	for _, kv := range img.vars(nil) {
		name, _, _ := strings.Cut(kv, "=")
		if name != "UV_LINK_MODE" && name != "UV_CACHE_DIR" && name != "PIXI_CACHE_DIR" {
			vars = append(vars, kv)
		}
	}
	vars = append(vars, "PATH="+strings.Join(img.paths(nil), ":")+":$PATH")
	p("ENV %s\n\n", strings.Join(vars, " \\\n    "))
	// Every step after the install of the managers uses the cache mount,
	// so no cache lands in the image.
	build := fmt.Sprintf("UV_CACHE_DIR=%[1]s/uv PIXI_CACHE_DIR=%[1]s/pixi UV_LINK_MODE=copy UV_NO_PROGRESS=1 PIXI_NO_PROGRESS=true PIXI_COLOR=never", buildCache)
	cacheMount := "--mount=type=cache,target=" + buildCache
	var marker []string
	if s.python != "" {
		src, err := s.context(s.python)
		if err != nil {
			return "", err
		}
		lock, err := s.context(s.uvLock())
		if err != nil {
			return "", err
		}
		dst := filepath.Join(img.dir, "src", "python") + "/"
		files := []string{src, lock}
		// uv reads the version of Python from .python-version when it
		// exists, so the image gets the same interpreter.
		if pin := filepath.Join(filepath.Dir(s.python), ".python-version"); exists(pin) {
			rel, err := s.context(pin)
			if err != nil {
				return "", err
			}
			files = append(files, rel)
		}
		p("COPY %s %s\n", strings.Join(files, " "), dst)
		p("RUN %s \\\n    %s \\\n    uv sync --frozen --no-install-project --project %s\n\n", cacheMount, build, strings.TrimSuffix(dst, "/"))
		marker = append(marker,
			fmt.Sprintf(`py=$(uv pip list --format json --python %s)`, img.venv()),
			fmt.Sprintf(`pyver=$(sed -n 's/^version_info = //p' %s/pyvenv.cfg)`, img.venv()))
	}
	if s.tools != "" {
		src, err := s.context(s.tools)
		if err != nil {
			return "", err
		}
		lock, err := s.context(s.pixiLock())
		if err != nil {
			return "", err
		}
		dst := filepath.Join(img.dir, "src", "tools")
		manifest := filepath.Join(dst, filepath.Base(s.tools))
		p("COPY %s %s %s/\n", src, lock, dst)
		p("RUN %s \\\n    %s \\\n    pixi install --frozen --manifest-path %s\n\n", cacheMount, build, manifest)
		marker = append(marker, fmt.Sprintf(`tools=$(pixi list --json --frozen --manifest-path %s)`, manifest))
	}
	for _, c := range s.prepare {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		// A prepare command runs in the root of the workspace at run time.
		// The image has no workspace, so it runs in dir.
		if err := enc.Encode([]string{"/bin/sh", "-c", "export " + build + "; cd " + img.dir + " && " + c}); err != nil {
			return "", err
		}
		p("RUN %s %s", cacheMount, buf.String())
	}
	if len(s.prepare) > 0 {
		p("\n")
	}
	// The marker records the digest and the versions, as an install at
	// run time does, so the brief and fw doctor read the image.
	doc := fmt.Sprintf(`{"digest":"%s","placement":"image","ready":"%%s","uv":"%%s","pixi":"%%s"`, digest)
	args := []string{`"$(date -u +%Y-%m-%dT%H:%M:%SZ)"`, `"$(uv --version)"`, `"$(pixi --version)"`}
	if s.python != "" {
		doc += fmt.Sprintf(`,"python":{"manifest":"%s","version":"%%s","env":"%s","packages":%%s}`, s.rel(s.python), img.venv())
		args = append(args, `"$pyver"`, `"$py"`)
	}
	if s.tools != "" {
		doc += fmt.Sprintf(`,"tools":{"manifest":"%s","prefix":"%s","packages":%%s}`, s.rel(s.tools), img.imagePixiPrefix())
		args = append(args, `"$tools"`)
	}
	doc += fmt.Sprintf(`,"prepare":%d}\n`, len(s.prepare))
	p("RUN %s set -eu; export %s; \\\n", cacheMount, build)
	for _, line := range marker {
		p("    %s; \\\n", line)
	}
	p("    printf '%s' \\\n      %s \\\n      > %s\n", doc, strings.Join(args, " "), img.markerPath())
	return b.String(), nil
}

// context returns a path of the workspace relative to its root, which is
// the build context of the Dockerfile.
func (s *spec) context(p string) (string, error) {
	rel, err := filepath.Rel(s.root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("env: %s is outside the root of the workspace %s, which is the build context", p, s.root)
	}
	if !safePath.MatchString(rel) {
		return "", fmt.Errorf("env: %s holds characters that a Dockerfile must quote", rel)
	}
	return rel, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
