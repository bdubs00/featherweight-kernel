GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/bdubs00/featherweight-kernel
# TAGS selects the drivers of the binary: empty for every driver, or lean
# plus the tags of the drivers that the build needs, for example
# TAGS=lean,nats,openai,oidc (docs/ARCHITECTURE.md section 15). The tag
# propose adds fw routine propose to a lean build. The tag memory_sqlite
# adds the SQLite FTS5 index of the memory to either build.
TAGS    ?=

.PHONY: build test vet fmt run image image-toolbox clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -tags "$(TAGS)" -ldflags "-s -w -X main.version=$(VERSION)" -o bin/fw ./cmd/fw

test:
	$(GO) test -race ./...
	$(GO) test -race -tags memory_sqlite ./memory/ ./tools/builtin/ ./surface/native/

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

run: build
	./bin/fw serve

image:
	docker build --build-arg VERSION=$(VERSION) --build-arg TAGS=$(TAGS) -t $(IMAGE):static -t $(IMAGE):$(VERSION) .

image-toolbox:
	docker build --build-arg VERSION=$(VERSION) --build-arg TAGS=$(TAGS) -f Dockerfile.toolbox -t $(IMAGE):toolbox .

clean:
	rm -rf bin
