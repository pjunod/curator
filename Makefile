# Monarr — build orchestration.
# `make build` produces ./bin/monarr with the web UI embedded.

# Tag releases as vX.Y.Z. Derivation lives in scripts/version.sh (shared
# with the Docker build): git describe when the release tags are present,
# else the VERSION file — bumped with every tag — as "0.3.0+g<hash>". A
# tagless clone never shows a raw commit hash as its version.
VERSION ?= $(shell sh scripts/version.sh)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X github.com/monarr-media/monarr/internal/buildinfo.Version=$(VERSION) -X github.com/monarr-media/monarr/internal/buildinfo.Commit=$(COMMIT)

# Tool versions are pinned here; Makefile targets and CI must agree.
SQLC         := github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
OAPI_CODEGEN := github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0
GOLANGCI     := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

.PHONY: all build go-build web test test-web test-e2e lint vet fmt gen gen-sqlc gen-api tidy clean dev-api dev-web docker hooks

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

clean:
	rm -rf bin web/dist/assets web/dist/index.html
