# Media Viewer Server — Linux container image
# Build context must be the repo root (needs backend/ sources + Deployable/web):
#   docker build -f backend/Dockerfile -t media-viewer:latest .
#
# Layout inside the image (relative config paths resolve against the exe dir):
#   /app/server   static Go binary
#   /app/web/     web frontend
#   /app/data/    writable volume: .thumbnails/, index/
#   /app/config.json  bind-mounted (server reads AND writes this file)

# ---- build stage ----
FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ .
# CGO_ENABLED=0 -> fully static binary, runs on any libc (alpine, distroless...)
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/server .

# ---- runtime stage ----
FROM alpine:3.20
# ffmpeg/ffprobe: optional runtime deps for video thumbnails + responsive WebP.
# wget: used by the HEALTHCHECK. tzdata: correct mtime display.
RUN apk add --no-cache ffmpeg ca-certificates tzdata wget \
    && addgroup -g 1000 appgroup \
    && adduser -D -u 1000 -G appgroup appuser

WORKDIR /app
COPY --from=build /out/server /app/server
COPY Deployable/web/ /app/web/

RUN mkdir -p /app/data/.thumbnails /app/data/index \
    && chown -R appuser:appgroup /app/data

USER appuser

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD wget -qO- http://127.0.0.1:3000/api/health || exit 1

# -notray: console foreground mode -> logs stream to `docker logs`.
# Never use -daemon in a container (PID 1 / signal semantics).
ENTRYPOINT ["/app/server", "-config", "/app/config.json", "-notray"]