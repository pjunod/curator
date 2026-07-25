# Monarr — build orchestration.
# `make build` produces ./bin/monarr with the web UI embedded.

# The version is the VERSION file, verbatim (scripts/version.sh, shared with
# the Docker build) — no git describe, no commit suffix, no dependency on
# tags. Bump VERSION to cut a release. The commit is stamped separately.
VERSION ?= $(shell sh scripts/version.sh)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X github.com/monarr-media/monarr/internal/buildinfo.Version=$(VERSION) -X github.com/monarr-media/monarr/internal/buildinfo.Commit=$(COMMIT)

# Tool versions are pinned here; Makefile targets and CI must agree.
SQLC         := github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
OAPI_CODEGEN := github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0
GOLANGCI     := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

.PHONY: all build go-build web test test-web test-e2e coverage coverage-html lint vet fmt gen gen-sqlc gen-api tidy clean dev-api dev-web docker bootstrap hooks

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

test-e2e: build
	cd test/e2e && npm ci && npm test

# Coverage is measured and rendered here rather than uploaded to a service.
# -coverpkg=./... is the load-bearing flag: without it a package is in the
# profile only if it owns a _test.go file, which measures where the tests live
# rather than what they cover. scripts/coverage-badge.sh then drops generated
# code and writes the badge — both rules live in that script so this target, CI
# and the README badge cannot report three different numbers.
coverage:
	go test -covermode=atomic -coverpkg=./... -coverprofile=coverage.out ./...
	@printf 'coverage: %s%% (badge: coverage.svg)\n' "$$(sh scripts/coverage-badge.sh coverage.out coverage.svg)"

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

tidy:
	go mod tidy

dev-api:
	go run ./cmd/monarr

dev-web:
	cd web && npm run dev

docker:
	docker build -t monarr:dev .

# Create the host directories a compose deployment bind-mounts, owned by
# PUID:PGID, before the first `up`. Reads deploy/.env if present.
bootstrap:
	deploy/bootstrap.sh

clean:
	rm -rf bin web/dist/assets web/dist/index.html
