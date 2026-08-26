COVERAGE_MIN ?= 80.0
GO_TOOLCHAIN ?= $(shell sed -n 's/^FROM golang:\([0-9.]*\)-alpine.*/go\1/p' Dockerfile | head -1)
# Read back from the Dockerfile so the timeout has one definition, the same way
# GO_TOOLCHAIN does. Go's 10m default is too tight: internal/store needs ~6m
# under -race on an idle runner and ~13m on a loaded workstation, so the
# default turns ordinary CPU contention into a failure that reads as a deadlock.
GO_TEST_TIMEOUT ?= $(shell sed -n 's/^ARG GO_TEST_TIMEOUT=\(.*\)$$/\1/p' Dockerfile | head -1)

.PHONY: test docker-quality build run smoke mutate backup-smoke fmt-check tooling-check version-check lint vuln coverage race benchmark-notify quality

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
	@test -n "$(SOURCE_VERSION)" || { \
		echo "could not read const Current from internal/version/version.go" >&2; \
		echo "an empty value would silently switch the version check back off" >&2; \
		exit 1; \
	}
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

# The disaster-recovery scripts are 279 lines executed by no gate. Both #266
# and #231 were bugs in them, and both fixes were locked in by grepping the
# shell source rather than running it. Local only and not wired into CI: it
# stops the running app and manipulates the real data volume, which is not
# something a CI job should be doing to a developer machine or a runner.
backup-smoke:
	scripts/backup.sh $(BACKUP_ARCHIVE)
	scripts/restore-smoke.sh $(BACKUP_ARCHIVE)

BACKUP_ARCHIVE ?= artist-trackarr-backup.tgz

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
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1 run ./...

vuln:
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...

coverage:
	go test ./internal/... -p 1 -timeout $(GO_TEST_TIMEOUT) -count=1 -coverprofile=coverage.out
	@coverage="$$(go tool cover -func=coverage.out | awk '/^total:/ {gsub("%", "", $$NF); print $$NF}')"; \
	if [ -z "$$coverage" ]; then \
		echo "could not determine test coverage" >&2; \
		exit 1; \
	fi; \
	printf 'total coverage: %s%% (minimum: %s%%)\n' "$$coverage" "$(COVERAGE_MIN)"; \
	awk -v coverage="$$coverage" -v minimum="$(COVERAGE_MIN)" 'BEGIN { exit !(coverage >= minimum) }'

benchmark-notify:
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go test ./internal/notify -timeout $(GO_TEST_TIMEOUT) -run '^$$' -bench '^BenchmarkShoutrrrSendSerialization$$' -benchmem -count=1

# The 80% coverage floor used to run in CI and nowhere else, so a local
# `make quality` could pass on a change CI would reject. `make coverage` runs
# the suite exactly as the plain `go test` step did, and then checks the floor,
# so this costs no extra suite run. It scopes to ./internal/..., which leaves
# cmd/server to the -race step below rather than dropping it.
race:
	go test -race -p 1 -timeout $(GO_TEST_TIMEOUT) ./... -count=1

quality: fmt-check tooling-check version-check
	go vet ./...
	$(MAKE) coverage
	$(MAKE) race
	$(MAKE) lint vuln
