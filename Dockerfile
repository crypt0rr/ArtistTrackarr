# syntax=docker/dockerfile:1.26@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS dependencies
WORKDIR /src
RUN apk add --no-cache ca-certificates tzdata
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

FROM dependencies AS test
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... && go test ./...

FROM dependencies AS build
COPY . .
ARG APP_VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/crypt0rr/artist-tracker/internal/version.Current=${APP_VERSION}" -o /out/artist-trackarr ./cmd/server

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
