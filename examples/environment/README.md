# Example environment

A Python package (`pyyaml`, through uv) and a tool (`jq`, through pixi) for the block `environment` of `fw.yaml` (docs/ARCHITECTURE.md section 18). The lock files need the network, so this directory holds the manifests alone: run `uv lock` and `pixi lock` here to write `uv.lock` and `pixi.lock`, and keep both next to their manifests. Then `fw doctor` shows the state and the egress hosts, and `fw run` or `fw serve` installs the environment under `./.environment`.
