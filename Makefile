# Monarr — build orchestration.
# `make build` produces ./bin/monarr with the web UI embedded.

# Tag releases as vX.Y.Z; describe yields "v0.1.0" or "v0.1.0-3-gabc1234".
# The leading v is stripped here because the UI adds its own.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

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
	go run $(SQLC) generate

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
