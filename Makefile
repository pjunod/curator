# Monarr — build orchestration.
# `make build` produces ./bin/monarr with the web UI embedded.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

# Generator versions are pinned here; `make gen` and CI must agree.
SQLC         := github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
OAPI_CODEGEN := github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0

.PHONY: all build go-build web test vet fmt gen gen-sqlc gen-api tidy clean dev-api dev-web docker

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
