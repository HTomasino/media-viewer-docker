#!/usr/bin/env bash
# Build the media-viewer Linux image and publish it to GHCR.
# Usage (from repo root or anywhere):
#   ./docker/build.sh            # -> ghcr.io/htomasino/media-viewer:latest
#   ./docker/build.sh v1.2.3     # -> ...:v1.2.3 (also retags latest)
# Requires: docker login ghcr.io (PAT with write:packages).
set -euo pipefail

cd "$(dirname "$0")/.."
TAG="${1:-latest}"
IMAGE="ghcr.io/htomasino/media-viewer"

docker build -f backend/Dockerfile -t "${IMAGE}:${TAG}" .
docker push "${IMAGE}:${TAG}"
if [ "$TAG" != "latest" ]; then
  docker tag "${IMAGE}:${TAG}" "${IMAGE}:latest"
  docker push "${IMAGE}:latest"
fi
echo "Published ${IMAGE}:${TAG}"