COVERAGE_MIN ?= 80.0
GO_TOOLCHAIN ?= $(shell sed -n 's/^FROM golang:\([0-9.]*\)-alpine.*/go\1/p' Dockerfile | head -1)

.PHONY: test docker-quality build run smoke mutate fmt-check tooling-check version-check lint vuln coverage benchmark-notify quality

test:
	docker build --target test .

docker-quality:
	docker build --target quality .

build:
	docker build -t artist-trackarr:local .

run:
	$(MAKE) build
	ARTIST_TRACKARR_IMAGE=artist-trackarr:local docker compose up

# Run the container smoke test against a locally built image, exactly as CI
# does. Without this target the script was reachable only through CI, so a
# defect in the harness itself - it is a curl client driving a real container,
# not Go code - escaped gofmt, vet, the test suite, the race detector, the
# linters and govulncheck alike. One did: a curl invocation forced POST across a
# 303 redirect and failed with 405 rather than exercising its assertion.
# SMOKE_EXPECTED_VERSION turns on the smoke script's version-identity check,
# which asserts the running container renders the version it was built from and
# that no "vdev" build leaked into the UI. That block shipped with #102 and
# had never once executed, because no caller ever set the variable.
SOURCE_VERSION = $(shell sed -n 's/^const Current = "\(.*\)"$$/\1/p' internal/version/version.go)

smoke:
	docker build -t artist-trackarr:smoke .
	SMOKE_EXPECTED_VERSION=$(SOURCE_VERSION) scripts/container-smoke.sh artist-trackarr:smoke

# Reintroduce every catalogued defect and confirm its guard still fails. This
# project proves each fix by reverting it and watching the test fail; done by
# hand that proof happens once and then decays, which is how six of the
# forty-four v0.58.0 findings turned out to be residuals of earlier fixes.
# Slower than the unit suite because it rebuilds per mutation, so it is a
# deliberate step rather than part of `quality`.
mutate:
	python3 scripts/mutate.py

fmt-check:
	@test -z "$$(gofmt -l internal cmd)"

version-check:
	@if [ -n "$(VERSION)" ]; then \
		scripts/check-version-docs.sh "$(VERSION)"; \
	else \
		scripts/check-version-docs.sh; \
	fi

tooling-check:
	@scripts/check-renovate-toolchain.sh
	@marker=$$(printf '@%s' latest); if git grep -n -F "$$marker" -- .github Dockerfile scripts; then \
		echo "unpinned tool reference found" >&2; \
		exit 1; \
	fi
	@if git grep -n -E '(^|[^[:alnum:]_-])(golang|alpine):[^@[:space:]]+([[:space:]]|$$)' -- Dockerfile scripts; then \
		echo "unpinned container helper reference found" >&2; \
		exit 1; \
	fi
	@docker_go=$$(sed -n 's/^FROM golang:\([0-9.]*\)-alpine.*/\1/p' Dockerfile | head -1); \
	ci_go=$$(sed -n "s/.*go-version: '\([^']*\)'.*/\1/p" .github/workflows/docker.yml | head -1); \
	module_go=$$(sed -n 's/^go //p' go.mod | cut -d. -f1-2); \
	docker_line=$$(printf '%s' "$$docker_go" | cut -d. -f1-2); \
	if [ -z "$$docker_go" ] || [ -z "$$ci_go" ] || [ -z "$$module_go" ] || [ "$$docker_go" != "$$ci_go" ] || [ "$$docker_line" != "$$module_go" ]; then \
		echo "Go toolchain drift: module=$$module_go Docker=$$docker_go CI=$$ci_go" >&2; \
		exit 1; \
	fi

lint:
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.0 run ./...

vuln:
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...

coverage:
	go test ./internal/... -p 1 -count=1 -coverprofile=coverage.out
	@coverage="$$(go tool cover -func=coverage.out | awk '/^total:/ {gsub("%", "", $$NF); print $$NF}')"; \
	if [ -z "$$coverage" ]; then \
		echo "could not determine test coverage" >&2; \
		exit 1; \
	fi; \
	printf 'total coverage: %s%% (minimum: %s%%)\n' "$$coverage" "$(COVERAGE_MIN)"; \
	awk -v coverage="$$coverage" -v minimum="$(COVERAGE_MIN)" 'BEGIN { exit !(coverage >= minimum) }'

benchmark-notify:
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go test ./internal/notify -run '^$$' -bench '^BenchmarkShoutrrrSendSerialization$$' -benchmem -count=1

quality: fmt-check tooling-check version-check
	go vet ./...
	go test ./... -count=1
	go test -race -p 1 ./... -count=1
	$(MAKE) lint vuln
