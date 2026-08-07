COVERAGE_MIN ?= 80.0

.PHONY: test build run fmt-check tooling-check lint vuln coverage quality

test:
	docker build --target test .

build:
	docker build -t artist-trackarr:local .

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

coverage:
	go test ./internal/... -p 1 -count=1 -coverprofile=coverage.out
	@coverage="$$(go tool cover -func=coverage.out | awk '/^total:/ {gsub("%", "", $$NF); print $$NF}')"; \
	if [ -z "$$coverage" ]; then \
		echo "could not determine test coverage" >&2; \
		exit 1; \
	fi; \
	printf 'total coverage: %s%% (minimum: %s%%)\n' "$$coverage" "$(COVERAGE_MIN)"; \
	awk -v coverage="$$coverage" -v minimum="$(COVERAGE_MIN)" 'BEGIN { exit !(coverage >= minimum) }'

quality: fmt-check tooling-check
	go vet ./...
	go test ./... -count=1
	go test -race ./... -count=1
	$(MAKE) lint vuln
