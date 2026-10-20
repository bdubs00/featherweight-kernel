# syntax=docker/dockerfile:1
# fw:static. A static binary on a distroless base, for agents that use HTTP,
# MCP, and A2A and need no shell. Build with:
#   docker build -t ghcr.io/bdubs00/featherweight-kernel:static .
ARG GO_VERSION=1.27
FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# TAGS selects the drivers: empty for every driver, or lean plus the tags of
# the drivers that the image needs, for example lean,nats,openai,oidc
# (docs/ARCHITECTURE.md section 15).
ARG TAGS=""
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -tags "${TAGS}" -ldflags "-s -w -X main.version=${VERSION}" -o /out/usr/local/bin/fw ./cmd/fw \
 && ln -s fw /out/usr/local/bin/ax-task-runner

FROM gcr.io/distroless/static-debian12:latest
# The base holds the tool user nonroot, uid and gid 65532, in /etc/passwd
# and /etc/group, with the home /home/nonroot. Under ax, fw starts the
# processes of tools as that user (tools.user, docs/ARCHITECTURE.md section
# 7.12) and stays root itself. Dockerfile.toolbox creates the same user.
COPY --from=build /out/ /
# ax probes port 80; plain deployments use 8080.
EXPOSE 80 8080
ENTRYPOINT ["/usr/local/bin/fw"]
CMD ["serve"]
