# Monarr — build orchestration.
# `make build` produces ./bin/monarr with the web UI embedded.

# The version is the VERSION file, verbatim (scripts/version.sh, shared with
# the Docker build) — no git describe, no commit suffix, no dependency on
# tags. Bump VERSION to cut a release. The commit is stamped separately.
VERSION ?= $(shell sh scripts/version.sh)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X github.com/pjunod/monarr/internal/buildinfo.Version=$(VERSION) -X github.com/pjunod/monarr/internal/buildinfo.Commit=$(COMMIT)

# Tool versions are pinned here; Makefile targets and CI must agree.
SQLC         := github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
OAPI_CODEGEN := github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0
GOLANGCI     := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

.PHONY: all build go-build web test test-web test-mobile mobile-export test-e2e coverage coverage-gate coverage-html lint vet fmt gen gen-sqlc gen-api release release-check tidy clean dev-api dev-web dev-mobile docker bootstrap hooks

all: build

build: web go-build

go-build:
	mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/monarr ./cmd/monarr

web: web/node_modules
	cd web && npm run build

web/node_modules: web/package-lock.json
	cd web && npm ci

test:
	go test ./...

test-web: web/node_modules
	cd web && npm run -s test

mobile/node_modules: mobile/package-lock.json
	cd mobile && npm ci

test-mobile: mobile/node_modules
	cd mobile && npm run check

mobile-export: mobile/node_modules
	cd mobile && npm run doctor && npm run export

test-e2e: build
	cd test/e2e && npm ci && npm test

# Coverage is measured and rendered here rather than uploaded to a service.
# -coverpkg=./... is the load-bearing flag: without it a package is in the
# profile only if it owns a _test.go file, which measures where the tests live
# rather than what they cover. scripts/coverage-badge.sh then drops generated
# code and writes the badge — both rules live in that script so this target, CI
# and the README badge cannot report three different numbers.
#
# -count=1 is load-bearing, not habit. `go test` caches coverage profiles, and
# with -coverpkg=./... a cached package's profile still contains blocks for
# every OTHER package — measured against the source as it was when that entry
# was cached. Edit a file and re-run, and the merge sees two disjoint sets of
# line ranges for it: the denominator inflates and the percentage collapses.
# It read 71.9% against a true 86.5% here, which is exactly the kind of wrong
# number a coverage gate must never produce. CI is immune (fresh checkout, no
# cache); a laptop is not.
coverage:
	go test -count=1 -covermode=atomic -coverpkg=./... -coverprofile=coverage.out ./...
	@printf 'coverage: %s%% (badge: coverage.svg)\n' "$$(sh scripts/coverage-badge.sh coverage.out coverage.svg)"
	@sh scripts/coverage-gate.sh coverage.out

# The gate alone, for CI and for re-checking without re-running the suite.
# The floor is ./COVERAGE_MIN (a ratchet); the target is in the script.
coverage-gate:
	@sh scripts/coverage-gate.sh coverage.out

# The line-by-line view, for finding what to test next.
coverage-html: coverage
	go tool cover -html=coverage.out -o coverage.html
	@echo 'open coverage.html'

lint:
	go run $(GOLANGCI) run
	@unformatted="$$(gofmt -l . | grep -v node_modules || true)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

hooks:
	git config core.hooksPath .githooks
	chmod +x .githooks/*
	@echo "git hooks installed (pre-commit: fmt+vet+typecheck, pre-push: unit tests)"

vet:
	go vet ./...

fmt:
	gofmt -w $$(git ls-files '*.go' | grep -v '/gen/')

gen: gen-sqlc gen-api

gen-sqlc:
	go run $(SQLC) -f internal/infra/sqlite/sqlc.yaml generate

gen-api:
	go run $(OAPI_CODEGEN) -config internal/api/oapi-codegen.yaml internal/api/openapi.yaml

# Cut a version. VERSION is the single source of truth (scripts/version.sh),
# and it sat at 0.4.0 across 39 commits because nothing ever asked for a bump.
#   make release BUMP=minor
release:
	@sh scripts/release.sh $(or $(BUMP),patch)

# What is unreleased. Run it before deciding the bump — the answer is usually
# "more than you thought".
release-check:
	@tag="$$(git describe --tags --abbrev=0 2>/dev/null || echo '')"; \
	base="$$tag"; \
	if [ -z "$$base" ]; then base="$$(git log -1 --format=%H -- VERSION)"; fi; \
	if [ -z "$$base" ]; then base="$$(git rev-list --max-parents=0 HEAD)"; fi; \
	printf 'VERSION %s · last tag %s · counting from %s\n' \
	  "$$(cat VERSION)" "$${tag:-none}" "$$(git log -1 --format=%h $$base)"; \
	printf 'unreleased: %s commits, %s of them feat/fix\n' \
	  "$$(git rev-list --count $$base..HEAD)" \
	  "$$(git log --format=%s $$base..HEAD | grep -cE '^(feat|fix)' || true)"; \
	git log --format='  %h %s' $$base..HEAD | head -20

tidy:
	go mod tidy

dev-api:
	go run ./cmd/monarr

dev-web:
	cd web && npm run dev

dev-mobile: mobile/node_modules
	cd mobile && npm start

docker:
	docker build -t monarr:dev .

# Create the host directories a compose deployment bind-mounts, owned by
# PUID:PGID, before the first `up`. Reads deploy/.env if present.
bootstrap:
	deploy/bootstrap.sh

clean:
	rm -rf bin web/dist/assets web/dist/index.html
