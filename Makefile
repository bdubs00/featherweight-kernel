GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/bdubs00/featherweight-kernel

.PHONY: build test vet fmt run image image-toolbox clean

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
