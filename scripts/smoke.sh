#!/usr/bin/env bash
# Smoke-test a built Next Chapter image the way it runs in production: a
# read-only root filesystem, a fresh data volume, and generated secrets.
#
# Usage: scripts/smoke.sh IMAGE [EXPECTED_VERSION]
#
# With EXPECTED_VERSION (for example 1.2.3), also checks that the binary
# reports that version. Used by both CI and the publish workflow.
set -euo pipefail

image=${1:?usage: scripts/smoke.sh IMAGE [EXPECTED_VERSION]}
expected=${2:-}
name="next-chapter-smoke-$$"

cleanup() {
  status=$?
  if [ "$status" -ne 0 ] && docker container inspect "$name" >/dev/null 2>&1; then
    docker logs "$name" 2>&1 | tail -50 || true
  fi
  docker rm -fv "$name" >/dev/null 2>&1 || true
  exit "$status"
}
trap cleanup EXIT

reported=$(docker run --rm --read-only "$image" version)
echo "$reported"
if [ -n "$expected" ] && [ "$reported" != "Next Chapter $expected (Go)" ]; then
  echo "error: image reports \"$reported\", expected version $expected" >&2
  exit 1
fi

docker run -d --name "$name" -p 127.0.0.1::8000 --read-only --tmpfs /tmp \
  -e APP_PASSWORD="$(openssl rand -hex 24)" -e APP_SECRET_KEY="$(openssl rand -hex 32)" \
  "$image" >/dev/null
port=$(docker port "$name" 8000/tcp | head -1 | sed 's/.*://')
base="http://127.0.0.1:$port"

for _ in $(seq 1 30); do
  if curl --fail --silent "$base/healthz" >/dev/null; then
    break
  fi
  sleep 1
done
curl --fail --silent --show-error "$base/healthz"
echo

# The image's HEALTHCHECK command, and a page that needs the database and
# templates.
docker exec "$name" /next-chapter healthcheck
code=$(curl --silent --output /dev/null --write-out '%{http_code}' "$base/login")
if [ "$code" != 200 ]; then
  echo "error: GET /login returned $code" >&2
  exit 1
fi
echo "smoke test passed: $image"
