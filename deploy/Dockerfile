# ---- standalone local encoder ----
FROM rust:1.97.1-bookworm AS embed
WORKDIR /src/tools/curator-embed
COPY tools/curator-embed/Cargo.toml tools/curator-embed/Cargo.lock ./
COPY tools/curator-embed/src ./src
RUN --mount=type=cache,target=/usr/local/cargo/registry --mount=type=cache,target=/usr/local/cargo/git cargo build --locked --release

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
    -ldflags "-s -w -X github.com/pjunod/monarr/internal/buildinfo.Version=$V -X github.com/pjunod/monarr/internal/buildinfo.Commit=$C" \
    -o /monarr ./cmd/monarr
# A /data skeleton owned by the default runtime user. Docker seeds a fresh
# NAMED volume from the image's directory — contents and ownership — so this
# is what makes `docker run` and named-volume compose work with no host-side
# setup at all. It does NOT help bind mounts: a bind mount always takes the
# host directory's ownership, and if that directory is missing the daemon
# creates it as root. Nothing in an image can fix that; deploy/bootstrap.sh
# is the answer there.
RUN mkdir -p /data-skel/backups && chown -R 1000:1000 /data-skel

# ---- Cinema's Jellyfin FFmpeg 8 and its complete runtime dependencies ----
# Match Cinema/plurx: the signed Jellyfin Bookworm repository and package major.
FROM debian:bookworm-slim AS media-tools
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
 && install -d /etc/apt/keyrings \
 && curl -fsSL https://repo.jellyfin.org/jellyfin_team.gpg.key \
    | gpg --dearmor -o /etc/apt/keyrings/jellyfin.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/jellyfin.gpg] https://repo.jellyfin.org/debian bookworm main" \
    > /etc/apt/sources.list.d/jellyfin.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends jellyfin-ffmpeg8 \
 && /usr/lib/jellyfin-ffmpeg/ffmpeg -hide_banner -decoders 2>&1 \
    | grep -Eq '^[[:space:]]*A[^[:space:]]*[[:space:]]+ac4[[:space:]]' \
 && /usr/lib/jellyfin-ffmpeg/ffmpeg -hide_banner -bsfs 2>&1 | grep -qx dovi_rpu \
 && /usr/lib/jellyfin-ffmpeg/ffmpeg -hide_banner -h filter=tonemapx 2>&1 \
    | grep -q '^[[:space:]]*apply_dovi[[:space:]]' \
 && mkdir -p /runtime/usr/lib /runtime/usr/bin /runtime/usr/share/doc \
 && cp -a /usr/lib/jellyfin-ffmpeg /runtime/usr/lib/ \
 && ldd /usr/lib/jellyfin-ffmpeg/ffprobe /usr/lib/jellyfin-ffmpeg/ffmpeg \
    | awk '$2 == "=>" && $3 ~ /^\// { print $3 } $1 ~ /^\// && $1 !~ /:$/ { print $1 }' \
    | sort -u \
    | xargs -r cp --parents -L -t /runtime \
 && ln -s /usr/lib/jellyfin-ffmpeg/ffmpeg /runtime/usr/bin/ffmpeg \
 && ln -s /usr/lib/jellyfin-ffmpeg/ffprobe /runtime/usr/bin/ffprobe \
 && cp -a /usr/share/doc/. /runtime/usr/share/doc/ \
 && rm -rf /var/lib/apt/lists/*

# Prove the bundled binaries and libraries work in the actual distroless base.
FROM gcr.io/distroless/cc-debian12 AS media-runtime
COPY --from=media-tools /runtime/ /
RUN ["/usr/lib/jellyfin-ffmpeg/ffprobe", "-version"]
RUN ["/usr/lib/jellyfin-ffmpeg/ffmpeg", "-version"]

# ---- runtime ----
FROM media-runtime AS runtime
COPY --from=embed /src/tools/curator-embed/target/release/curator-embed /curator-embed
COPY --from=build /monarr /monarr
COPY --from=build --chown=1000:1000 /data-skel /data
ENV MONARR_DATA_DIR=/data \
    MONARR_PORT=7676 \
    MONARR_LOG_FORMAT=json \
    MONARR_FFPROBE=/usr/lib/jellyfin-ffmpeg/ffprobe
VOLUME /data
EXPOSE 7676
# Never root by default. Compose overrides this with ${PUID}:${PGID}; if you
# do override it, the host directory must be owned by that same uid/gid.
USER 1000:1000
ENTRYPOINT ["/monarr"]
