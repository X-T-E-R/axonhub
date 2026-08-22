# Managed observability storage

AxonHub now separates request-log skeleton rows from large request-body bytes. This is an additive, dual-read change: existing inline JSON and external-storage rows remain readable and are not rewritten at startup.

## Exact storage outcomes

| Setting | New rows |
| --- | --- |
| `store_request_body=false` | `requests.request_body` is `{}` and no managed parent payload is admitted. |
| `store_request_body=true` | With primary database storage, canonical inbound bytes are stored in `observability_payloads`; `requests.request_body_payload_id` references them and the required legacy `requests.request_body` column is `{}`. Non-primary external storage keeps its existing object key/disposition path. |
| `store_execution_request_body` missing | Inherits `store_request_body`, preserving the old shared-switch behavior. |
| `store_execution_request_body=false` | Execution skeletons remain, but their request-body reference is absent unless a channel override enables it. |
| `store_execution_request_body=true` | Final provider request bytes use the same request-group catalog. Byte-identical retries/failovers reference one physical payload; transformed, pass-through, model/URL/header override variants are byte-compared and stored once per distinct byte sequence. Channel overrides take precedence. |
| `managed_observability_hard_mib` / `managed_observability_low_mib` missing | Capacity mode is disabled for a legacy rollout. Exact managed dedup still applies. |

Hash and length only select candidates; AxonHub compares the final persisted bytes before reuse. Managed payload metadata (SHA-256, byte length, disposition) remains on the skeleton after capacity eviction. Response-body and chunk switches retain their existing shared parent/execution semantics.

Capacity mode marks the primary-database Request/RequestExecution group as managed even when parent and execution request-body capture are disabled. Accounting uses conservative payload charges plus the actual database-resident response/header/chunk JSON and fixed skeleton margins for an explicit allowlist: managed Requests, RequestExecutions, their payloads, related Trace/Thread diagnostic rows, and Usage Logs. Non-primary external object bytes are outside this database capacity budget. Cleanup evicts payload bytes first while preserving digest/length/disposition; only if the low watermark still cannot be reached does it delete complete terminal managed request groups and their Usage rows through the existing integrity-checked deletion path. It never selects accounts, API keys, projects, routes, providers, channels, or system configuration. Successful request groups are cleaned before groups containing failed evidence; a group is active if either its parent or any child execution is pending/processing, even if contradictory parent metadata says terminal. Active groups are not selected. PostgreSQL workers contend on one non-waiting session advisory lock held on a dedicated connection across retention, capacity cleanup, and ordinary VACUUM. SQLite/MySQL are explicitly single-process only for this ownership guarantee.

Pressure is fail-open: provider forwarding and request/execution skeleton transitions continue, while new primary-database request bodies, request headers, response bodies, response chunks, and Usage Logs may be skipped. Structured `managed_observability_*` signals, `managed_observability_failures_total{component,reason}`, and the singleton `managed_observability_states.last_error` component value describe admission, owner-lock, unlock, and cleanup degradation. Public health reports this component state but remains healthy; health/liveness must not be wired to fail solely because capacity is under pressure.

Runtime auto-migration deliberately uses `WithForeignKeys(false)`. Declared Ent cascade/set-null edges are therefore not the lifecycle authority. Retention and capacity deletion explicitly remove request-owned payload catalog rows and reconcile their charged bytes in the same application transaction; legacy inline groups pass through the same deletion path without requiring a payload row.

## Staged deployment

The designated database migration owner should perform these gates in order:

1. Keep the temporary policy (`store_request_body=false`, `store_response_body=true`, `store_chunks=false`, `live_preview=false`, Requests 1 day, Usage Logs 30 days).
2. Deploy the dual-read schema/code and verify legacy inline and external rows through GraphQL and diagnostics. Leave capacity fields unset.
3. Set `managed_observability_hard_mib=10240` and `managed_observability_low_mib=8192`; verify only one PostgreSQL GC owner and observe admission/GC signals.
4. Enable `store_request_body=true` and `store_execution_request_body=true`. Automatic exact dedup prevents identical execution copies. A per-channel execution override can still disable or enable capture.

For a disposable release proof, run `scripts/postgres-managed-observability-integration.sh`. It accepts no DSN, provisions fresh PostgreSQL, executes the real runtime migration and application admission/GC/locking paths for eight hard/low cycles, terminates the actual advisory-lock session to prove release, then runs the bounded 24 MiB heap/TOAST/index/VACUUM reuse smoke. It never connects to an operator-supplied database.

The 24-hour Requests and 30-day Usage retention values are maxima. At the observed parent-body rate (about 15 GiB per 25 hours), a 10/8 GiB budget will evict successful request payloads before 24 hours.

## Existing ~30 GB transition and physical space

Do not run a destructive startup rewrite and do not use routine `VACUUM FULL`. Normal deletes plus `VACUUM (ANALYZE)` make heap/TOAST pages reusable and should make the managed payload relation's allocation plateau, but they do not promise to return historical relation files to the filesystem.

## Stale streaming executions

Retention cleanup automatically reconciles a historical `pending` or `processing`
execution only when its parent Request is terminal, both rows are beyond the
configured retention cutoff, and the execution has not been updated for at least
24 hours. These are independent conditions: creation age uses the configured
retention cutoff, while `updated_at` is compared only with the 24-hour activity
grace. PostgreSQL instances share the existing non-waiting advisory owner and
recheck each row under transaction locks before changing it to `failed`; retained
Trace/Thread groups remain protected. Reconciliation does not delete external
artifacts. The ordinary terminal-record cleanup path performs any later deletion.
The 24-hour grace is the cross-instance safety authority; the process-local
`LiveStreamRegistry` is only local evidence. SQLite and MySQL deployments therefore
retain the documented single-process ownership limitation.

For a one-time backlog, run
`scripts/postgres-reconcile-stale-executions.sql` in small batches. It is dry-run
by default, uses a 2-second `lock_timeout` and 30-second `statement_timeout`, and
requires `-v apply=true` to update rows. It requires no service pause and embeds
no connection details. Repeat until `candidate_count` is zero, then let normal
retention cleanup delete eligible terminal groups. Ordinary `VACUUM (ANALYZE)`
makes the reclaimed pages reusable; it does **not** return a historical 23 GB
relation high-water allocation to the filesystem. Filesystem shrinkage requires
a separately planned relation rewrite with its own lock and free-space budget.

For a one-time filesystem shrink, use a separately approved online rewrite tool such as `pg_repack`, execution-first:

1. Preflight extension/tool version compatibility, blocking DDL, long transactions, replica lag, and backup restore. Measure `pg_total_relation_size`, heap, TOAST, indexes, dead tuples, reusable pages, and filesystem free space separately.
2. Reserve peak free space for the relation plus indexes and at least 20% operational headroom. Abort before starting if that bound is unavailable.
3. First rewrite the legacy `request_executions` relation, verify reads and forwarding, then rewrite `requests`. Run only one rewrite owner and monitor locks/latency.
4. Keep the pre-rewrite backup/snapshot until row counts, legacy reads, diagnostics, and storage measurements pass. The rollback boundary is the table swap: before it, cancel and retain the old relation; after it, restore from the validated backup rather than attempting an in-place reverse rewrite.

Rolling back to an old binary is safe only before new managed-only rows are admitted. After parent/execution capture is enabled, an old binary sees required legacy `{}` placeholders; materialize managed payloads back to the legacy representation (or retain the dual-read binary) before an application downgrade.
