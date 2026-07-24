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
# A /data skeleton owned by the default runtime user. Docker seeds a fresh
# NAMED volume from the image's directory — contents and ownership — so this
# is what makes `docker run` and named-volume compose work with no host-side
# setup at all. It does NOT help bind mounts: a bind mount always takes the
# host directory's ownership, and if that directory is missing the daemon
# creates it as root. Nothing in an image can fix that; deploy/bootstrap.sh
# is the answer there.
RUN mkdir -p /data-skel/backups && chown -R 1000:1000 /data-skel

# ---- runtime ----
FROM gcr.io/distroless/static-debian12
COPY --from=build /monarr /monarr
COPY --from=build --chown=1000:1000 /data-skel /data
ENV MONARR_DATA_DIR=/data \
    MONARR_PORT=7676 \
    MONARR_LOG_FORMAT=json
VOLUME /data
EXPOSE 7676
# Never root by default. Compose overrides this with ${PUID}:${PGID}; if you
# do override it, the host directory must be owned by that same uid/gid.
USER 1000:1000
ENTRYPOINT ["/monarr"]
