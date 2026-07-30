# syntax=docker/dockerfile:1.7
FROM golang:1.26-alpine AS dependencies
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
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/artist-tracker ./cmd/server

FROM alpine:3.22 AS runtime
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S -g 10001 tracker && adduser -S -D -H -u 10001 -G tracker tracker && \
    mkdir /data && chown tracker:tracker /data
COPY --from=build /out/artist-tracker /usr/local/bin/artist-tracker
USER tracker
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/artist-tracker"]
