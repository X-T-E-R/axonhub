#!/usr/bin/env bash
set -euo pipefail

# AxonHub CCP Crucible — 3-Gate Pre-Merge Verification Suite
# Runs on Linux with PostgreSQL 18 + Redis 7 + Go 1.26+.
#
# Usage:
#   scripts/ccp-crucible/run-all.sh [--gate a|b|c|all] [--baseline-bin PATH] [--candidate-bin PATH] [--out-dir DIR]

GATE="all"
BASELINE_BIN=""
CANDIDATE_BIN=""
OUT_DIR="/tmp/crucible-out"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --gate) GATE="$2"; shift 2 ;;
    --baseline-bin) BASELINE_BIN="$2"; shift 2 ;;
    --candidate-bin) CANDIDATE_BIN="$2"; shift 2 ;;
    --out-dir) OUT_DIR="$2"; shift 2 ;;
    *) echo "Unknown arg: $1" >&2; exit 2 ;;
  esac
done

export PATH="/usr/local/go/bin:/usr/lib/postgresql/18/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:${PATH:-}"
mkdir -p "$OUT_DIR"

PG_DIR="/tmp/crucible-pg18-$$"
PG_PORT=55432
REDIS_PORT=6399
REDIS_PID=""
PG_STARTED=0

cleanup() {
  if [[ "$PG_STARTED" -eq 1 ]]; then
    su - postgres -c "/usr/lib/postgresql/18/bin/pg_ctl -D '$PG_DIR' -m immediate stop" >/dev/null 2>&1 || true
    rm -rf "$PG_DIR"
  fi
  if [[ -n "$REDIS_PID" ]]; then
    kill "$REDIS_PID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

start_infra() {
  echo "=== Starting disposable PostgreSQL 18 (port $PG_PORT) and Redis 7 (port $REDIS_PORT) ==="
  rm -rf "$PG_DIR"
  mkdir -p "$PG_DIR"
  chown postgres:postgres "$PG_DIR"
  su - postgres -c "/usr/lib/postgresql/18/bin/initdb -D '$PG_DIR' --username=postgres --auth=trust >/dev/null"
  su - postgres -c "/usr/lib/postgresql/18/bin/pg_ctl -D '$PG_DIR' -o '-p $PG_PORT -k /tmp -c max_connections=200 -c shared_buffers=256MB' -l '$PG_DIR/pg18.log' start >/dev/null"
  PG_STARTED=1
  su - postgres -c "/usr/lib/postgresql/18/bin/psql -h 127.0.0.1 -p $PG_PORT -U postgres -c \"ALTER USER postgres WITH PASSWORD 'postgres';\" >/dev/null"
  su - postgres -c "/usr/lib/postgresql/18/bin/createdb -h 127.0.0.1 -p $PG_PORT -U postgres axonhub_test"

  redis-server --port "$REDIS_PORT" --save "" --appendonly no --logfile "$OUT_DIR/redis.log" &
  REDIS_PID=$!
  for _ in $(seq 1 30); do
    if redis-cli -p "$REDIS_PORT" ping >/dev/null 2>&1; then break; fi
    sleep 0.1
  done

  export AXONHUB_TEST_PG_DSN="postgres://postgres:postgres@127.0.0.1:${PG_PORT}/axonhub_test?sslmode=disable"
  export AXONHUB_TEST_REDIS_URL="redis://127.0.0.1:${REDIS_PORT}/0"
  export AXONHUB_TEST_PG_DISPOSABLE="1"
  echo "Infra ready: PG=$AXONHUB_TEST_PG_DSN REDIS=$AXONHUB_TEST_REDIS_URL"
}

assert_zero_skips() {
  local label="$1"
  local jsonlog="$2"
  python3 - "$label" "$jsonlog" <<'PY'
import json, sys
label, path = sys.argv[1], sys.argv[2]
passes, fails, skips = 0, 0, 0
skipped_names, failed_names = [], []
with open(path, encoding="utf-8", errors="replace") as f:
    for line in f:
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            ev = json.loads(line)
        except Exception:
            continue
        if not ev.get("Test"):
            continue
        act = ev.get("Action")
        if act == "pass":
            passes += 1
        elif act == "fail":
            fails += 1
            failed_names.append(f"{ev.get('Package','')}:{ev['Test']}")
        elif act == "skip":
            skips += 1
            skipped_names.append(f"{ev.get('Package','')}:{ev['Test']}")
print(f"[{label}] passes={passes} fails={fails} skips={skips}")
if passes == 0 or fails != 0 or skips != 0:
    if failed_names:
        print(f"FAILED: {failed_names}", file=sys.stderr)
    if skipped_names:
        print(f"SKIPPED: {skipped_names}", file=sys.stderr)
    if passes == 0:
        print(f"ERROR: [{label}] matched 0 tests!", file=sys.stderr)
    sys.exit(1)
PY
}

fresh_pg_db() {
  local dbname="$1"
  su - postgres -c "/usr/lib/postgresql/18/bin/dropdb --if-exists -h 127.0.0.1 -p $PG_PORT -U postgres '$dbname'" >/dev/null
  su - postgres -c "/usr/lib/postgresql/18/bin/createdb -h 127.0.0.1 -p $PG_PORT -U postgres '$dbname'" >/dev/null
  echo "postgres://postgres:postgres@127.0.0.1:${PG_PORT}/${dbname}?sslmode=disable"
}

run_gate_a() {
  echo "============================================================"
  echo "GATE A: Zero-Skip PG18 + Redis + Full Module & Race Verification"
  echo "============================================================"
  mkdir -p "$OUT_DIR/gate-a"

  echo "--- A1: llm/ full -race ---"
  (cd "$REPO_ROOT/llm" && go test -race -count=1 ./...) 2>&1 | tee "$OUT_DIR/gate-a/llm_race.log"

  echo "--- A2: root full go test ./... (isolated unit/SQLite suite) ---"
  (cd "$REPO_ROOT" && env -u AXONHUB_TEST_PG_DSN -u AXONHUB_TEST_REDIS_URL -u AXONHUB_TEST_PG_DISPOSABLE go test -count=1 ./...) 2>&1 | tee "$OUT_DIR/gate-a/root_full.log"

  echo "--- A3: PG18 + Redis zero-skip -race (isolated DB per suite) ---"
  local dsn
  redis-cli -p "$REDIS_PORT" FLUSHALL >/dev/null
  dsn="$(fresh_pg_db axonhub_a3_server)"
  (cd "$REPO_ROOT" && AXONHUB_TEST_PG_DISPOSABLE=1 AXONHUB_TEST_PG_DSN="$dsn" AXONHUB_TEST_REDIS_URL="$AXONHUB_TEST_REDIS_URL" \
    go test -json -race -count=1 \
    -run 'TestClusterCompactAccountingAndLocalSystemSnapshotPG18|TestClusterTwoHTTPServicesAuthorizationAndOwnership|TestClusterSharedPGAndRedisControlOnlyOAuthState' \
    ./internal/server) > "$OUT_DIR/gate-a/pg18_server_race.json"
  assert_zero_skips "A3-PG18-server" "$OUT_DIR/gate-a/pg18_server_race.json"

  for gc_test in TestManagedObservabilityPostgresApplicationContract TestManagedLedgerRevisionUpgradeAndMutations TestManagedCapacityPostgresRepairContracts TestWorker_runVacuum_Postgres; do
    dsn="$(fresh_pg_db "axonhub_a3_${gc_test,,}")"
    (cd "$REPO_ROOT" && AXONHUB_TEST_PG_DISPOSABLE=1 AXONHUB_TEST_PG_DSN="$dsn" \
      go test -json -race -count=1 -run "^${gc_test}" ./internal/server/gc) > "$OUT_DIR/gate-a/pg18_gc_${gc_test}.json"
    assert_zero_skips "A3-PG18-gc-${gc_test}" "$OUT_DIR/gate-a/pg18_gc_${gc_test}.json"
  done

  dsn="$(fresh_pg_db axonhub_a3_biz)"
  (cd "$REPO_ROOT" && AXONHUB_TEST_PG_DISPOSABLE=1 AXONHUB_TEST_PG_DSN="$dsn" AXONHUB_TEST_REDIS_URL="$AXONHUB_TEST_REDIS_URL" \
    go test -json -race -count=1 \
    -run 'TestAccountedUsagePostgresCommitAfterSnapshot|TestClusterPendingUsage|TestClusterAccountingAdditiveMigrationAndDrainedRollback|TestClusterUsage|TestForwardingObservation|TestChannelService_MetricsConcurrentReadWriteRace' \
    ./internal/server/biz) > "$OUT_DIR/gate-a/pg18_biz_race.json"
  assert_zero_skips "A3-PG18-biz" "$OUT_DIR/gate-a/pg18_biz_race.json"

  dsn="$(fresh_pg_db axonhub_a3_orch)"
  (cd "$REPO_ROOT" && AXONHUB_TEST_PG_DISPOSABLE=1 AXONHUB_TEST_PG_DSN="$dsn" \
    go test -json -race -count=1 -run 'TestForwardingObservationCoreBarrier' ./internal/server/orchestrator) > "$OUT_DIR/gate-a/pg18_orch_race.json"
  assert_zero_skips "A3-PG18-orch" "$OUT_DIR/gate-a/pg18_orch_race.json"

  redis-cli -p "$REDIS_PORT" FLUSHALL >/dev/null
  (cd "$REPO_ROOT" && AXONHUB_TEST_REDIS_URL="$AXONHUB_TEST_REDIS_URL" \
    go test -json -race -count=1 ./internal/pkg/xcache/...) > "$OUT_DIR/gate-a/xcache_race.json"
  assert_zero_skips "A3-xcache" "$OUT_DIR/gate-a/xcache_race.json"

  (cd "$REPO_ROOT" && env -u AXONHUB_TEST_PG_DSN -u AXONHUB_TEST_REDIS_URL \
    go test -race -count=1 ./internal/server/orchestrator/... ./internal/server/api/... ./internal/server/middleware/...) \
    2>&1 | tee "$OUT_DIR/gate-a/core_race.log"

  echo "--- A4: integration_test/{openai,anthropic,gemini} zero-skip ---"
  for mod in openai anthropic gemini; do
    (cd "$REPO_ROOT/integration_test/$mod" && go test -json -count=1 ./...) > "$OUT_DIR/gate-a/integration_${mod}.json"
    assert_zero_skips "A4-integration-$mod" "$OUT_DIR/gate-a/integration_${mod}.json"
  done
  echo "GATE A: PASS"
}

run_gate_b() {
  echo "============================================================"
  echo "GATE B: Wire Rewrite & Failover Contract Matrix (-race)"
  echo "============================================================"
  mkdir -p "$OUT_DIR/gate-b"

  (cd "$REPO_ROOT" && go test -json -race -count=1 \
    -run 'TestFullPipeline_WireRewriteEncodingsAndFailoverMatrix|TestDirectPassThrough|TestShallowInbound|TestStreamOptionsAttemptIsolation|TestRawStreamObserver_AnthropicPartialUsageMergeAndResponsesIncomplete|TestChannelService_MetricsConcurrentReadWriteRace' \
    ./internal/server ./internal/server/orchestrator ./internal/server/biz) > "$OUT_DIR/gate-b/server_matrix_race.json"
  assert_zero_skips "B1-server-matrix" "$OUT_DIR/gate-b/server_matrix_race.json"

  (cd "$REPO_ROOT/llm" && go test -json -race -count=1 \
    -run 'TestHTTP2EarlyResponseAndCancelRace|TestRedirect307And308BodyReplay|TestReadLimitedBodySizedTruncatedUnexpectedEOF' \
    ./httpclient/...) > "$OUT_DIR/gate-b/httpclient_race.json"
  assert_zero_skips "B2-httpclient" "$OUT_DIR/gate-b/httpclient_race.json"

  (cd "$REPO_ROOT/llm" && go test -json -race -count=1 ./pipeline/...) > "$OUT_DIR/gate-b/pipeline_race.json"
  assert_zero_skips "B3-pipeline" "$OUT_DIR/gate-b/pipeline_race.json"

  echo "GATE B: PASS"
}

run_bench_case() {
  local tag="$1"
  local bin="$2"
  local scenario="$3"
  shift 3
  local dir="$OUT_DIR/gate-c/${tag}/${scenario}"
  mkdir -p "$dir"
  echo "--- Gate C [$tag / $scenario] ---"
  "$BENCH_BIN" --sut "$bin" --scenario "$scenario" --out "$dir" "$@" > "$dir/stdout.json"
  if [[ ! -s "$dir/midstream_heap.pb.gz" ]]; then
    echo "ERROR: missing or empty $dir/midstream_heap.pb.gz" >&2
    exit 1
  fi
  go tool pprof -top -nodecount=15 -inuse_space "$dir/midstream_heap.pb.gz" > "$dir/top_midstream.txt"
}

run_gate_c() {
  echo "============================================================"
  echo "GATE C: Chaos & Hard-Ceiling Linux A/B Benchmark (Wire-Verified)"
  echo "============================================================"
  mkdir -p "$OUT_DIR/gate-c"

  BENCH_BIN="$OUT_DIR/gate-c/crucible-bench"
  echo "Building crucible-bench..."
  (cd "$REPO_ROOT" && go build -o "$BENCH_BIN" ./scripts/ccp-crucible/bench)

  if [[ -z "$CANDIDATE_BIN" ]]; then
    CANDIDATE_BIN="$OUT_DIR/gate-c/axonhub-candidate"
    echo "Building axonhub-candidate..."
    (cd "$REPO_ROOT" && CGO_ENABLED=1 go build -o "$CANDIDATE_BIN" ./cmd/axonhub)
  fi

  # A/B comparison scenarios (identical flags + active upstream wire contract verification)
  if [[ -n "$BASELINE_BIN" && -x "$BASELINE_BIN" ]]; then
    echo "=== Running Baseline ($BASELINE_BIN) A/B Scenarios ==="
    run_bench_case baseline "$BASELINE_BIN" anthropic_plain --concurrency 128 --body-mib 2.5 --events 2000 --hold-sec 5 --verify-wire=true
    run_bench_case baseline "$BASELINE_BIN" anthropic_zstd  --concurrency 128 --body-mib 2.5 --events 2000 --hold-sec 5 --verify-wire=true
    run_bench_case baseline "$BASELINE_BIN" anthropic_256   --concurrency 256 --body-mib 2.5 --events 2000 --hold-sec 5 --verify-wire=true
    run_bench_case baseline "$BASELINE_BIN" openai_chat     --concurrency 128 --body-mib 2.5 --events 2000 --hold-sec 5 --verify-wire=true
    run_bench_case baseline "$BASELINE_BIN" openai_responses --concurrency 128 --body-mib 2.5 --events 2000 --hold-sec 5 --verify-wire=true
    run_bench_case baseline "$BASELINE_BIN" gomemlimit_220  --concurrency 128 --body-mib 2.5 --events 2000 --hold-sec 5 --gomemlimit 220MiB --verify-wire=true
  fi

  echo "=== Running Candidate ($CANDIDATE_BIN) Full Scenarios ==="
  run_bench_case candidate "$CANDIDATE_BIN" anthropic_plain   --concurrency 128 --body-mib 2.5 --events 2000 --hold-sec 5 --verify-wire=true
  run_bench_case candidate "$CANDIDATE_BIN" anthropic_zstd    --concurrency 128 --body-mib 2.5 --events 2000 --hold-sec 5 --verify-wire=true
  run_bench_case candidate "$CANDIDATE_BIN" anthropic_256     --concurrency 256 --body-mib 2.5 --events 2000 --hold-sec 5 --verify-wire=true
  run_bench_case candidate "$CANDIDATE_BIN" openai_chat       --concurrency 128 --body-mib 2.5 --events 2000 --hold-sec 5 --verify-wire=true
  run_bench_case candidate "$CANDIDATE_BIN" openai_responses  --concurrency 128 --body-mib 2.5 --events 2000 --hold-sec 5 --verify-wire=true
  run_bench_case candidate "$CANDIDATE_BIN" gomemlimit_220    --concurrency 128 --body-mib 2.5 --events 2000 --hold-sec 5 --gomemlimit 220MiB --verify-wire=true
  run_bench_case candidate "$CANDIDATE_BIN" anthropic_chaos   --concurrency 128 --body-mib 2.5 --events 2000 --hold-sec 5 --verify-wire=true --inject-precontent-error=true
  run_bench_case candidate "$CANDIDATE_BIN" anthropic_threads --concurrency 64  --body-mib 0.25 --events 400 --hold-sec 3 --verify-wire=true
  run_bench_case candidate "$CANDIDATE_BIN" dcz_multi         --concurrency 64  --body-mib 2.5 --events 800  --hold-sec 3 --verify-wire=true

  # Live PG18 + Redis scenario using a fresh database so schema/init is clean
  su - postgres -c "/usr/lib/postgresql/18/bin/dropdb --if-exists -h 127.0.0.1 -p $PG_PORT -U postgres axonhub_crucible_bench"
  su - postgres -c "/usr/lib/postgresql/18/bin/createdb -h 127.0.0.1 -p $PG_PORT -U postgres axonhub_crucible_bench"
  redis-cli -p "$REDIS_PORT" FLUSHALL >/dev/null
  run_bench_case candidate "$CANDIDATE_BIN" compact_accounting \
    --concurrency 128 --body-mib 2.5 --events 2000 --hold-sec 5 --verify-wire=true \
    --pg-dsn "postgres://postgres:postgres@127.0.0.1:${PG_PORT}/axonhub_crucible_bench?sslmode=disable" \
    --redis-url "$AXONHUB_TEST_REDIS_URL"

  echo "GATE C: PASS"
}

if [[ "$GATE" == "a" || "$GATE" == "c" || "$GATE" == "all" ]]; then
  start_infra
fi

case "$GATE" in
  a) run_gate_a ;;
  b) run_gate_b ;;
  c) run_gate_c ;;
  all)
    run_gate_a
    run_gate_b
    run_gate_c
    ;;
  *) echo "Invalid --gate: $GATE (use a|b|c|all)" >&2; exit 2 ;;
esac

echo "=== CCP CRUCIBLE ($GATE) COMPLETED SUCCESSFULLY ==="
