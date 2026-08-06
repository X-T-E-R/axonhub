#!/usr/bin/env bash
set -euo pipefail

# Disposable-only application-contract proof. This wrapper never accepts a
# database URL: it provisions a fresh PostgreSQL container, runs the real
# runtime migration/biz/GC/lock paths, destroys it, then runs the bounded
# 24 MiB relation/TOAST/VACUUM reuse smoke.
if [[ $# -ne 0 ]]; then
  echo "usage: $0 (no DSN arguments; the script provisions disposable PostgreSQL)" >&2
  exit 2
fi
if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required for the disposable PostgreSQL integration proof" >&2
  exit 2
fi
if ! command -v go >/dev/null 2>&1; then
  echo "go is required for the application-contract integration proof" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
container="axonhub-managed-app-$RANDOM-$$"
cleanup() { docker rm -f "$container" >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker run -d --rm --name "$container" \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=axonhub_managed \
  -p 127.0.0.1::5432 postgres:18-alpine >/dev/null

host_port="$(docker port "$container" 5432/tcp | awk -F: 'NR==1 {print $NF}')"
for _ in $(seq 1 90); do
  if docker exec "$container" pg_isready -U postgres -d axonhub_managed >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "$container" pg_isready -U postgres -d axonhub_managed >/dev/null

echo "== real runtime migration/admission/hysteresis/GC/ownership proof =="
(
  cd "$repo_root"
  AXONHUB_TEST_PG_DISPOSABLE=1 \
  AXONHUB_TEST_PG_DSN="postgres://postgres:postgres@127.0.0.1:${host_port}/axonhub_managed?sslmode=disable" \
    go test ./internal/server/gc \
      -run '^TestManagedObservabilityPostgresApplicationContract$' \
      -v -count=1
)

cleanup
trap - EXIT

echo "== bounded 24 MiB physical relation/TOAST/VACUUM reuse proof =="
(
  cd "$repo_root"
  AXONHUB_RELEASE_STORAGE_SMOKE=1 ./scripts/postgres-managed-observability-smoke.sh
)
