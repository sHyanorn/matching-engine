# syntax=docker/dockerfile:1
# Needs BuildKit (the default builder), for the mounts below and for
# WORKDIR to create /data owned by the image's nonroot user.

FROM --platform=$BUILDPLATFORM golang:1.24.7@sha256:2c5f7a0c252a17cf6aa30ddee15caa0f485ee29410a6ea64cddb62eea2b07bdf AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=. \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/engine ./cmd/loadgen

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/engine /out/loadgen /usr/local/bin/
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/engine", "-data=/data"]
