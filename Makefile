.PHONY: test build run fmt-check tooling-check lint vuln quality

test:
	docker build --target test .

build:
	docker build --build-arg APP_VERSION=$${APP_VERSION:-dev} -t artist-trackarr:local .

run:
	docker compose up --build

fmt-check:
	@test -z "$$(gofmt -l internal cmd)"

tooling-check:
	@marker=$$(printf '@%s' latest); if rg -n "$$marker" .github Dockerfile; then \
		echo "unpinned tool reference found" >&2; \
		exit 1; \
	fi

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...

quality: fmt-check tooling-check
	go vet ./...
	go test ./... -count=1
	go test -race ./... -count=1
	$(MAKE) lint vuln
