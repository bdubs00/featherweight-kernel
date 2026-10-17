GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/bdubs00/featherweight-kernel

.PHONY: build test vet fmt run e2e e2e-k3s e2e-k3s-install image image-toolbox clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/fw ./cmd/fw

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

run: build
	./bin/fw serve

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):static -t $(IMAGE):$(VERSION) .

image-toolbox:
	docker build --build-arg VERSION=$(VERSION) -f Dockerfile.toolbox -t $(IMAGE):toolbox .

clean:
	rm -rf bin

# The end-to-end tests against a real model. See e2e/README.md. The
# directory e2e/ holds the setup of one machine and stays out of git.
# e2e/loop.sh <local|k3s> -watch runs them again after each change.
e2e:
	GO=$(GO) e2e/loop.sh local

e2e-k3s:
	GO=$(GO) e2e/loop.sh k3s

e2e-k3s-install:
	GO=$(GO) e2e/k3s/install.sh
