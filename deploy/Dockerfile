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
# The version is the VERSION file (scripts/version.sh); an explicit
# --build-arg VERSION=… still wins. The commit is captured separately from
# git HEAD (the context includes .git) and shown on the Dashboard.
ARG VERSION=""
RUN apk add --no-cache git
RUN V="${VERSION:-$(sh scripts/version.sh)}" && \
    V="${V#v}" && \
    C="$(git rev-parse --short HEAD 2>/dev/null || echo none)" && \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/monarr-media/monarr/internal/buildinfo.Version=$V -X github.com/monarr-media/monarr/internal/buildinfo.Commit=$C" \
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
