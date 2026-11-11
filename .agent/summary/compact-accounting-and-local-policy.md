# Compact Local Accounting & Local System Snapshot Contract

## Overview

This change implements two complementary cross-region cluster capabilities for replaceable data nodes (such as GCP data-plane instances connected to the central `lag` PostgreSQL 18 + Redis control plane):

1. **Part A — Low-Frequency Compact Accounting Outbox (`cluster.compact_accounting`)**: Non-control data nodes persist ordinary non-quota API usage facts locally in a bounded SQLite WAL outbox (`synchronous=FULL`) and upload compact, digest-verified batches at a configurable low frequency (default `5m`) to the control node's single-transaction idempotent receiver, writing directly into the unified central `usage_logs` ledger without per-request WAN `pending_usages`, `requests`, `request_executions`, `traces`, or `threads` writes.
2. **Part B — Process-Local System Settings Snapshot + PG `LISTEN/NOTIFY` (`SystemService`)**: Activated `SystemService` instances serve non-transactional system-setting reads (including `RetryPolicy`, `ModelSettings`, `SecuritySettings`, `QuotaEnforcementSettings`, `PassThrough`, and `UpstreamErrorPolicy`) from an atomic in-memory snapshot with deep-cloned `RetryPolicy` values, synchronized across nodes via PostgreSQL `LISTEN/NOTIFY` (`axonhub_system_settings`) and primary-PG catch-up reloads after reconnects—eliminating per-request WAN Redis `GET`s and per-request PostgreSQL `SELECT`s on hot and error paths.

---

## Architecture & Key Contracts

### 1. Central Ledger Unification & Batch Idempotency (Part A)

- **Schema (`internal/ent/schema/usage_log.go`, `internal/ent/schema/compact_usage_batch.go`)**:
  - `usage_logs.request_id` is now optional/nullable in SQL (`Optional()` with nullable FK in Ent, nullable `ID` in GraphQL `UsageLog.requestID`, nullable `UsageLog.request` edge, and `z.string().nullable().optional()` in `frontend/src/features/requests/data/usage-logs-schema.ts`). All central dashboard, project, channel, model, cost, and quota aggregations continue reading one unified `usage_logs` table.
  - `usage_logs.event_id` is an optional unique UUID string indexing each compact accounting event for row-level idempotency.
  - `compact_usage_batches` stores durable batch receipts `(producer_uuid, seq_start, seq_end, event_count, payload_digest, node_id)`.
- **Single-Transaction Control Receiver (`POST /__axon/cluster/accounting/batches`)**:
  - Exposed **only** on the control node (`cluster.Config.IsControl()`).
  - Authenticated with domain-separated HMAC-SHA256 headers (`X-Axon-Cluster-Node-Id`, `X-Axon-Cluster-Timestamp`, `X-Axon-Cluster-Signature` derived from `SystemService.SecretKey` with a 5-minute replay skew bound—no user API keys or browser tokens).
  - `CompactAccountingService.IngestBatch` acquires a transaction-scoped PostgreSQL advisory lock (`pg_advisory_xact_lock`) keyed by `producer_uuid`, checks existing `compact_usage_batches` receipts:
    - Exact match on `(producer_uuid, seq_start, seq_end)` and `payload_digest` returns `200 OK` (`idempotent: true`) with zero duplicate `usage_logs` inserts.
    - Mismatched digest or overlapping sequence range returns `409 Conflict` (`ErrCompactBatchConflict`).
  - Inserts missing `usage_logs` rows (checked against `event_id` in-tx) preserving original `captured_at` as `created_at` (so time-window queries reflect actual execution time rather than upload time) and preserving `total_cost = NULL` when unpriced rather than coercing to `0`.
  - Records the `compact_usage_batches` receipt in the **same** PG commit, then triggers `UsageLogService.OnUsageLogCreated` post-commit.

### 2. Data-Node Admission Split, Local SQLite Outbox & Failure Fencing (Part A)

- **Explicit Quota Consistency Boundary**:
  - Compact local accounting applies **only** on non-control cluster nodes (`cluster.enabled && node_id != control_node && cluster.compact_accounting.enabled`) for non-video API requests whose authenticated `APIKey` has **no** configured hard quota (`!APIKeyHasHardQuota(apiKey)`).
  - Keys with configured hard quotas (`requests`, `total_tokens`, or `cost`) and async video tasks retain the real-time authoritative PostgreSQL path (`pending_usages` + `requests`/`request_executions`) so hard limits cannot overshoot silently across nodes.
  - When a request enters compact mode (`contexts.WithCompactAccounting`), `WithTrace` and `WithThread` middlewares still parse and attach trace/thread headers in memory for downstream session logic (such as `CyberSessionBlock`) without issuing remote `traces`/`threads` DB lookups or inserts.
- **Local Durable SQLite Outbox (`internal/server/biz/compact_accounting.go`)**:
  - Opens a local SQLite database in `WAL` mode with `synchronous=FULL` and `busy_timeout=5000`.
  - **Pre-dispatch spool reservation**: `CreateRequest` reserves local spool capacity (`MaxSpoolCount`, `MaxSpoolBytes`) before upstream provider dispatch. If the local spool is full or frozen for migration, the request is rejected before calling the upstream provider.
  - **Post-execution accounting fence**: After upstream execution completes (sync or stream), `Append` commits one contiguous `seq` event (`event_uuid`, `captured_at`, IDs, actual model, format, token breakdown, frozen nullable `total_cost` + `cost_items` + `price_reference_id`—never bodies, headers, prompts, or credentials). If the local SQLite write fails after upstream execution, AxonHub returns `*biz.ExecutedAccountingError`, preventing orchestrator retries/failovers, circuit-breaker penalties, or duplicate upstream charges.
  - **Single-active sealed batch**: `SealOrLoadNextBatch` persists the sealed `[seq_start, seq_end]` batch and canonical node-independent SHA-256 `payload_digest` in `outbox_sealed_batch` before upload, guaranteeing identical batch boundaries across lost ACKs, process crashes, or node migrations.

### 3. `/drain` Lifecycle & Replaceable-Node Migration CLI (Part A)

- **Maintenance `/status` & `/drain` (`internal/server/maintenance/local.go`, `cmd/axonhub/maintenance.go`)**:
  - Reports `unacked_compact_usage` separately alongside `nodePending` and `globalPending` (`pending_usages`).
  - `POST /drain` pauses ingress, waits for active HTTP/SSE requests and observation writers to stop, and immediately invokes `compactSvc.Flush(ctx)` (bypassing the periodic timer). Phase transitions to `drained` only when both `unacked_compact_usage == 0` and `nodePending == 0`; otherwise it surfaces an explicit drain error while retaining all unacknowledged facts in SQLite.
- **Frozen Backup Export & Import (`cmd/axonhub/compact_accounting.go`)**:
  - `axonhub compact-outbox status [--outbox <path>]` inspects producer UUID, sequence watermarks, sealed batch state, and unacknowledged counts/bytes.
  - `axonhub compact-outbox export --dir <dir> [--outbox <path>]` marks `outbox_meta.frozen = 1`, checkpoints WAL (`PRAGMA wal_checkpoint(TRUNCATE)`), executes `VACUUM INTO` to produce a clean single-file snapshot (`outbox.sqlite`), and writes `manifest.json` with SHA-256 and sequence metadata.
  - `axonhub compact-outbox import --dir <dir> [--outbox <path>]` verifies the SHA-256 manifest, refuses to overwrite any non-empty/mismatched target outbox (preventing split-brain dual writers), installs the snapshot with `frozen = 0` preserving original `producer_uuid`, `event_uuid`s, sequence numbers, and any sealed batch, ready for automatic startup catch-up on the replacement node.

### 4. Local Atomic System Snapshot & PG `LISTEN/NOTIFY` (Part B)

- **`systemSnapshotSyncer` (`internal/server/biz/system_snapshot.go`, `internal/server/biz/system.go`)**:
  - Started via Fx lifecycle (`internal/server/biz/fx_module.go`). Performs an initial primary-PG load of `systems` rows and pre-parses `RetryPolicy`.
  - Non-transactional `SystemService.getSystemValue` and `SystemService.RetryPolicy` read directly from `atomic.Pointer` snapshots in memory (`RetryPolicy` returns `cloneRetryPolicy` deep copies covering all nested slices and pointers), resulting in **zero** Redis `GET`s and **zero** per-request PostgreSQL `SELECT`s on inference hot paths and `upstream_error_policy.go` error paths.
  - `SystemService.setSystemValue` emits `SELECT pg_notify('axonhub_system_settings', $1)` on the same DB connection/transaction (so transactional mutations notify peers strictly upon commit), invalidates Redis cache for compatibility, and updates the local snapshot when non-transactional.
  - On PostgreSQL, `listenSession` opens a dedicated `pgx.Conn` extracted from the primary Ent driver, **executes `LISTEN axonhub_system_settings` first**, and **then** runs `reloadAll` from primary PG before entering `WaitForNotification`—closing any race window across startup or network disconnects/reconnects.
  - Shared dynamic state (`CyberSessionBlockCache`, `PromptProtectionRuleService` Redis watcher, APIKey/Channel caches, and quota queries) remains untouched on its authoritative Redis/PG backends.

---

## Configuration Knobs (`conf/conf.go` / `cluster.compact_accounting`)

```yaml
cluster:
  enabled: true
  node_id: "gcp-oregon-1"
  control_node: "lag"
  nodes:
    lag: "http://10.x.x.1:8090"
    gcp-oregon-1: "http://10.x.x.2:8090"
  compact_accounting:
    enabled: true
    outbox_path: ""          # defaults to axonhub-compact-outbox-<node_id>.db
    sync_interval: 5m        # low-frequency upload cadence
    request_timeout: 15s
    max_batch_count: 256
    max_batch_bytes: 262144  # 256 KiB
    max_spool_records: 65536
    max_spool_bytes: 67108864 # 64 MiB
    shared_secret: ""        # optional override; defaults to HMAC derived from SystemService.SecretKey
```

---

## Verification Summary

- **SQLite Unit & Fault-Injection Suite (`internal/server/biz/compact_accounting_test.go`, `cmd/axonhub/compact_accounting_test.go`)**:
  - Verified contiguous sequence assignment, crash/reopen replay of the exact sealed batch, duplicate-batch idempotency, digest/overlap `409` conflict fencing, spool-full pre-dispatch rejection, post-execution SQLite failure returning `*ExecutedAccountingError`, `VACUUM INTO` frozen export tamper detection + replacement import catch-up, CLI `status|export|import`, and concurrent `RetryPolicy` deep-clone mutation safety (`-race`).
- **Real PostgreSQL 18 + Redis Dual-Instance Cluster Integration (`internal/server/compact_accounting_pg_integration_test.go`)**:
  - Ran `TestClusterCompactAccountingAndLocalSystemSnapshotPG18` with `-race` against live `postgres:18.0-alpine` + `redis:7-alpine`:
    - Verified `0` Redis `GET`s and `0` per-request PG `systems` `SELECT`s across sync, SSE streaming, unpriced, and `502` upstream-error masking requests on data node B;
    - Verified `0` remote PG writes to `pending_usages`, `requests`, `request_executions`, `traces`, and `threads` for non-quota compact requests;
    - Verified GraphQL `updateRetryPolicy` on control node A propagates via PG `LISTEN/NOTIFY` to data node B, and simulated socket disconnect + missed update recovers via reconnect `LISTEN`-then-primary-reload;
    - Verified `CyberSessionBlock` (Redis) and `PromptProtectionRuleService` (Redis watcher) continue enforcing across nodes;
    - Verified lost-ACK network fault after control-PG commit replays idempotently with zero double-counting, preserving `request_id = NULL` and `total_cost = NULL` vs priced `total_cost` in PG and GraphQL `usageLogs`;
    - Verified hard-quota `APIKey` retains real-time authoritative PG enforcement (`403 quota_exceeded` on request 3/2);
    - Verified `/status` (`unacked_compact_usage`) + `POST /drain` immediate flush to `phase="drained"`, and frozen outbox export -> replacement import -> startup automatic replay.
