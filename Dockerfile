# syntax=docker/dockerfile:1.27@sha256:bde3983e9c939224420ddaf6b784cc30e09b035a4dea01f581230c50809f372e
FROM golang:1.27.1-alpine@sha256:3f6d04dc61331ee3c2fbbaad62d54412a84680f6a041d269a20a5270a078515b AS dependencies
WORKDIR /src
RUN apk add --no-cache ca-certificates tzdata
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Go's default per-package test timeout is 10m, and internal/store alone needs
# ~6m under -race on an idle CI runner. A loaded machine crosses the default
# and reports a timeout that looks exactly like a deadlock, which is how a
# green CI run and a red local `make quality` happened at the same commit.
# Set it well clear of the real figure while still bounding a genuine hang.
# The Makefile reads this value back out, the way it already does for the Go
# toolchain, so there is one definition of it.
ARG GO_TEST_TIMEOUT=30m

FROM dependencies AS test
ARG GO_TEST_TIMEOUT
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... && go test -p 1 -timeout $GO_TEST_TIMEOUT ./... -count=1

# The test stage is intentionally fast and is used by the regular image
# smoke-build. The quality stage is the reproducible, resource-bounded gate
# for local release validation: it serializes race tests and runs the same
# pinned lint and vulnerability tools as CI.
FROM dependencies AS quality
ARG GO_TEST_TIMEOUT
COPY . .
RUN apk add --no-cache build-base
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... && \
    go test -p 1 -timeout $GO_TEST_TIMEOUT ./... -count=1 && \
    go test -race -p 1 -timeout $GO_TEST_TIMEOUT ./... -count=1 && \
    go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run ./... && \
    go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...

FROM dependencies AS build
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/artist-trackarr ./cmd/server

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS runtime
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S -g 10001 tracker && adduser -S -D -H -u 10001 -G tracker tracker && \
    mkdir /data && chown tracker:tracker /data
ENV TZ=UTC
COPY --from=build /out/artist-trackarr /usr/local/bin/artist-trackarr
USER tracker
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/artist-trackarr"]
