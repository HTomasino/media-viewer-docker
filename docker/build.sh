#!/usr/bin/env bash
# Build the media-viewer Linux image.
# Usage (from repo root or anywhere):
#   ./docker/build.sh            # -> media-viewer:latest
#   ./docker/build.sh v1.2.3     # -> media-viewer:v1.2.3 (also tags latest)
set -euo pipefail

cd "$(dirname "$0")/.."
TAG="${1:-latest}"

docker build -f backend/Dockerfile -t "media-viewer:${TAG}" ${TAG:+$( [ "$TAG" != "latest" ] && echo "-t media-viewer:latest" || true )} .
echo "Built media-viewer:${TAG}"