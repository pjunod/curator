# ---- web UI ----
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- go binary ----
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
# Self-version from git (the context includes .git); an explicit
# --build-arg VERSION=… still wins, e.g. for tagged release builds.
ARG VERSION=""
RUN apk add --no-cache git
RUN V="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}" && \
    C="$(git rev-parse --short HEAD 2>/dev/null || echo none)" && \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=$V -X main.commit=$C" \
    -o /monarr ./cmd/monarr

# ---- runtime ----
FROM gcr.io/distroless/static-debian12
COPY --from=build /monarr /monarr
ENV MONARR_DATA_DIR=/data \
    MONARR_PORT=7676 \
    MONARR_LOG_FORMAT=json
VOLUME /data
EXPOSE 7676
ENTRYPOINT ["/monarr"]
