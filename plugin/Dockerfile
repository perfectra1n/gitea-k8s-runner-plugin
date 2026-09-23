# syntax=docker/dockerfile:1
# Build context: repository root (the plugin module depends on gen/ via go.work).
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.work ./
COPY gen/go.mod gen/go.sum gen/
COPY plugin/go.mod plugin/go.sum plugin/
RUN --mount=type=cache,target=/go/pkg/mod go mod download -C gen && go mod download -C plugin
COPY gen/ gen/
COPY plugin/ plugin/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/gitea-k8s-runner-plugin ./plugin/cmd/gitea-k8s-runner-plugin

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gitea-k8s-runner-plugin /usr/local/bin/gitea-k8s-runner-plugin
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/gitea-k8s-runner-plugin"]
