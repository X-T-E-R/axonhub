# Task49 accounting migration and recovery contract

The opt-in cluster mode adds an Ent-managed `pending_usages` table. Standalone mode keeps the current process-local pending path and does not query this table for quota admission.

## Atomic accounting

Each captured usage has a UUID nonce. Before publishing its asynchronous usage job, the executing node inserts one compact pending record into the authoritative PG. It contains the Key/project/channel IDs, capture time, actual token details and frozen nullable cost, plus the request observation nonce and compact fields needed to recover a missing parent. It never contains HTTP bodies, headers or credentials.

The usage worker obtains the pending row's write lock, creates `usage_logs` and deletes that pending row in one `RunInTransaction` commit. Quota reads both tables in one repeatable-read snapshot. It counts a fact from exactly one table even if handoff commits during the read. Capture time determines every existing quota window; unknown cost stays NULL. This remains actual-usage accounting, not dispatch reservation.

Chat streaming keeps the latest compact usage projection until the provider's `[DONE]` or clean EOF; intermediate usage frames do not freeze billing. Cluster semantic-completion confirmation remains pending until the stream owner settles final required capture. A transformer-synthesized terminal also waits for that same reader to consume the provider tail before exposing success or an accounting error; it does not wait for optional Close I/O. The existing grace duration is unchanged, and the reader retains normal EOF/cancellation ownership. Standalone semantic fallback is unchanged.

Pending publication failure still transfers the same frozen usage ID, capture time, tokens and nullable cost to the reserved required writer, which retries publication and atomic handoff. The request returns an accounting error after execution; HTTP fails, and SSE/WS withhold normal completion and expose a failed terminal. Neither the gateway nor callers may replay the provider operation. Existing standalone/no-quota behavior is unchanged. In cluster mode all real usage is durably projected, including Keys without quota, so a later quota change cannot omit already captured facts.

## Crash recovery

A node drains its own surviving pending records before starting its observation worker and accepting requests. It resolves the recorded request nonce against the existing primary-DB observation recovery path. If a crash lost the asynchronous parent, recovery creates a compact terminal request skeleton without body, headers or trace. The captured usage is then committed with the same atomic handoff. The recovered row remains attributed to the original Key and project. A pending row is never expired or deleted because of age.

Only one process may run a given node ID. Recovery is node-local ownership recovery, not a timeout lease allowing another live process to take over. Cross-node quota remains correct while an owner is offline because its pending rows stay visible.

## Upgrade and rollback procedure

1. Drain current inference and stop all old writers in a maintenance window. Preserve the existing PG recovery snapshot.
2. With the new binary on the control node, run the existing Ent migration path once. The change is additive; no existing usage rows, cost columns or indexes are rewritten. Data nodes keep `db.disable_auto_migration=true`.
3. Start both nodes with the same placement map, pure Redis namespace and authoritative PG (no replica). Verify the disposable PG18 migration/handoff/recovery tests before production admission.
4. After any new admission, preserve the latest authoritative PG, including usage and pending facts. To return to the old single-node binary, first stop admission and drain all node writers. Reconcile every node's surviving pending rows with the new binary, verify `pending_usages` is empty, then stop data nodes and enable the old reader against that same latest PG. Keep the additive table; the old binary ignores it.
5. If drain or reconciliation is blocked, keep admission stopped and retain the new reader or maintenance mode until handoff succeeds. Do not enable an old quota reader with pending rows, and do not restore a pre-admission snapshot: that would discard already executed usage and newer account/configuration changes.

Production schema writes and deployment remain outside this implementation run.
