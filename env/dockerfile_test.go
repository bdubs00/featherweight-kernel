//go:build unix

package env

import (
	"bytes"
	"strings"
	"testing"
)

// The Dockerfile installs pinned managers, copies the manifests and the
// locks from the build context, runs the commands of an install with dir
// inside the image, and writes the marker with the digest.
func TestDockerfile(t *testing.T) {
	f := newFixture(t, fullBlock)
	f.write(".python-version", "3.12\n")
	var b bytes.Buffer
	if err := writeDockerfile(f.cfg, "ghcr.io/example/fw:toolbox@sha256:0123", &b); err != nil {
		t.Fatal(err)
	}
	text := b.String()
	d := imageDir
	for _, want := range []string{
		"# syntax=docker/dockerfile:1",
		"FROM ghcr.io/example/fw:toolbox@sha256:0123\n",
		"https://github.com/astral-sh/uv/releases/download/" + uvVersion + "/uv-${arch}-unknown-linux-musl.tar.gz",
		"https://github.com/prefix-dev/pixi/releases/download/v" + pixiVersion + "/pixi-${arch}-unknown-linux-musl.tar.gz",
		"amd64) arch=x86_64; uv_sha=" + checksums["x86_64"]["uv"],
		"sha256sum -c -",
		"UV_PROJECT_ENVIRONMENT=" + d + "/venv",
		"PIXI_HOME=" + d + "/pixi",
		"PATH=" + d + "/venv/bin:" + d + "/src/tools/.pixi/envs/default/bin:",
		"COPY pyproject.toml uv.lock .python-version " + d + "/src/python/\n",
		"uv sync --frozen --no-install-project --project " + d + "/src/python\n",
		"COPY pixi.toml pixi.lock " + d + "/src/tools/\n",
		"pixi install --frozen --manifest-path " + d + "/src/tools/pixi.toml\n",
		`RUN --mount=type=cache,target=` + buildCache + ` ["/bin/sh","-c","export UV_CACHE_DIR=` + buildCache + `/uv PIXI_CACHE_DIR=` + buildCache + `/pixi UV_LINK_MODE=copy UV_NO_PROGRESS=1 PIXI_NO_PROGRESS=true PIXI_COLOR=never; cd ` + d + ` && jq --version > prepared.txt && echo \"$VIRTUAL_ENV\" >> prepared.txt"]`,
		`"digest":"` + f.s.digest() + `","placement":"image"`,
		"> " + d + "/.fw-env.json",
		"#     placement: image\n#     dir: " + d + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the Dockerfile lacks %q", want)
		}
	}
	if strings.Contains(text, f.root) {
		t.Error("the Dockerfile names a path of the build machine")
	}
	if strings.Contains(text, "ENV UV_CACHE_DIR") || strings.Contains(text, "    PIXI_CACHE_DIR=/") {
		t.Error("the image keeps a cache variable")
	}
	if err := writeDockerfile(f.cfg, "", &b); err == nil {
		t.Error("an empty base image passed")
	}
	out := newFixture(t, "python: /etc/pyproject.toml\n")
	if err := writeDockerfile(out.cfg, "debian", &b); err == nil || !strings.Contains(err.Error(), "outside the root of the workspace") {
		t.Errorf("a manifest outside the context: %v", err)
	}
	img := newFixture(t, "placement: image\ndir: /opt/agent-env\ntools: pixi.toml\n")
	b.Reset()
	if err := writeDockerfile(img.cfg, "debian", &b); err != nil || !strings.Contains(b.String(), "/opt/agent-env/src/tools/pixi.toml") || strings.Contains(b.String(), "uv sync") {
		t.Errorf("image block: %v\n%s", err, b.String())
	}
}
