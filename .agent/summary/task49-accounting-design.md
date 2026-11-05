# Task49 accounting: confirmed publication, WAN recovery and batch proposal

## Local candidate and release consumption

The product change is confined to `internal/server/biz/usage_log.go`: once the frozen pending fact has an acknowledged publication, its required job retries **handoff only**. A missing pending row after that acknowledgement means another attempt already consumed it. It must not be reinserted after a successful COMMIT whose acknowledgement was lost. An unacknowledged publication still retries the same UUID, timestamp, actual tokens and nullable cost; it never repeats the provider operation.

The independent source tree is `E:/kiki-wt/axon-multi-node-edge-20261104`, based on `8838762b`. The deployed release `f6cd6378` contains the same accounting files plus the accepted surrogate/maintenance changes. Consume this incremental commit onto the isolated f6cd release source, not the root unstable branch. No schema, quota API, default timeout, raw capability or provider configuration changes are included. Production has not loaded this candidate.

## Atomic accounting in the current product

Each capture publishes a UUID-keyed compact `pending_usages` row to the authoritative PG before queueing its usage job. Handoff locks that row, creates `usage_logs` and deletes pending in one `RunInTransaction` commit. Quota reads both tables in one repeatable-read snapshot. The previous retry unconditionally published pending again before handoff; after an ambiguous successful commit this recreated an already-consumed fact and temporarily doubled quota. Remembering the confirmed publication removes that extra state transition.

If publication fails or its ACK is unknown, the response reports an executed accounting error and the required writer retains the frozen fact for publication recovery. HTTP/SSE/WS must not label it not-executed or replay the provider. Once publication is confirmed, handoff failure remains an asynchronous persistence problem: retry the transaction, not capture or inference. Chat keeps the latest compact usage until DONE/clean EOF; required capture precedes semantic completion. Standalone semantic fallback, cancellation, nullable cost and capture-time quota windows stay unchanged.

Startup recovers the node's durable pending before admission. It finds the existing request by its observation nonce, or creates a compact terminal skeleton when the asynchronous parent was lost. Recovery neither reprices nor expires a fact. Only one process may own the node ID. Existing legacy-republished pending is reconciled through the same usage-existence check; startup never republishes it.

## What crosses the WAN today

The five shared capture switches being false removes ordinary inference request/response bodies, headers, chunks and live preview. It does not make the product a stats-only protocol.

| Path | Data and dependency | Source |
| --- | --- | --- |
| Synchronous usage capture | UUID, node, Key/project/channel IDs, actual model, source/format, capture/request time, request ID/observation nonce, token details, frozen nullable cost/items/price reference. No payloads or credentials in this row. | `biz/pending_usage.go:18–34` |
| Required FIFO | Fresh storage policy each job; request/execution inserts and terminal updates; usage handoff SQL. Request metadata includes model, stream/source, Key/project/trace, client IP and the effective routing-profile snapshot. Execution metadata includes channel, actual model, request URL, masked selected-key label, metrics and status. Profiles already held centrally can be duplicated per request. | `biz/forwarding_observation.go:287–319`, `biz/request.go:328–443,652–705` |
| Fresh authorization | Key/project lookup plus owner and membership checks for managed personal Keys; HTTP and each WS generation remain dependent on authoritative authorization. | `biz/auth.go:218`, `biz/portal_access.go:20–66` |
| Quota | Configured quotas perform a primary-PG repeatable-read aggregation of final and pending facts. No-quota admission skips this query, but still durably captures real usage. | `biz/quota.go:52–76`, `biz/pending_quota.go` |
| Other shared state | Channel/config/price loading, account/OAuth changes, async task records and shared Redis cache/watcher/limiter operations remain cross-node dependencies. A stats batching change alone does not remove them. | `biz/channel.go:229–295`, `biz/task_execution.go`, `cluster/config.go` |

The local measurement uses **real PostgreSQL 18.0**, two pools and a synthetic 140ms delay before each TCP protocol write plus dial; the writer pool keeps zero idle connections. One synthetic 17+3-token capture, handoff and stop took **4.458s**, opening three PG connections and sending **6,203 B / receiving 3,638 B**. These are application PG protocol bytes including SQL, prepared statements and authentication, not Ethernet/WireGuard bytes or a production-volume estimate. The measurement excludes request/execution creation, ordinary authentication and Redis. Cold PG round trips, not a large model response, dominate this sample.

## Loading a recovery timeout or fixed image

`AXONHUB_MANAGED_REQUEST_BODY_WRITER_ATTEMPT_TIMEOUT=8s` is an existing **startup-only mitigation candidate**, not the low-frequency architecture. The candidate keeps the 2s default. The local 8s test finishes an entire healthy cold-WAN capture and drain within that budget; it does not promise bounded draining through a database outage. Raising this value also raises the existing retry pause, because both use the same setting.

For the current f6cd process, `/drain` has a hard-coded 900s lifetime. On expiry `writer.Stop` cancels the worker and can abandon RAM lifecycle jobs, although durable PG pending survives. The CLI has no deadline argument, and extending the SSH/CLI timeout cannot change this. Keep an acknowledged maintenance pause while the writer makes progress; do not use pending=0 or activeHTTP=0 as a substitute for a drained writer. The deferred Sub2 carrier may retain that paused process and postpone Axon finalization; it must report `writersStopped:false` honestly.

An env change requires Compose recreate; `docker start` reuses the old env. A product fix also requires a new image built from **f6cd plus this accounting increment**. No build or rollout is authorized by this document.

The existing `current_owner()` accepts only same-ID/image, monotonically increasing StartedAt epochs. Do not weaken it or overwrite `candidate-compose-ack.json`, GO, original owner ACK, Task46 binding or prior claims. A future authorized recreate needs a separate append-only owner-transition contract:

1. Bind the exact previous `{id,imageID,startedAt,run,node}` and an explicit allowed immutable image/source plus the non-secret env change. Preserve the original private env; create a separate protected override.
2. Stop admission and obtain actual `phase=drained`, `writersStopped=true`, `nodePending=0`, then an acknowledged stop of that owner. Preserve the latest authoritative PG. If the old process cannot drain safely, keep it paused; this small fix does not provide a lossless RAM export/hot-swap route.
3. Under the existing run lock, write one recreate intent, prove the old owner is stopped, invoke Compose once, inspect the effective env/image/labels and health, and append a new-owner receipt. Only this explicitly authorized transition can change ID/image; ordinary restart retains the same-ID rule. There is never a second active writer for the same node.
4. Unknown recreate/ACK leaves the original intent and owner chain unchanged. Observe actual state and require an explicit adoption decision; do not repeat Compose/start or release ingress. Observation never manufactures an original ACK.
5. Resume later Task46 URL/restart/release work through its original plan and producer state. Recovered-old readers use a distinct recovery-owner role and the **latest** PG; they require completed pending reconciliation, not a pre-admission snapshot.

This increment does not modify the Task46 carrier.

## Proposed low-frequency compact accounting

**Decision requested before implementation:** introduce a mounted local SQLite outbox and central idempotent batch receipts, and choose quota consistency. Five minutes is a validation candidate, not a user-approved interval. No new outbox, schema or batch endpoint is implemented in this increment.

Use the existing embedded SQLite dependency rather than a new broker. One local durable transaction freezes the actual usage before reporting successful capture. Send one compact fact per billable operation so exact quota windows remain representable: event UUID, capture time, Key/project/channel IDs, actual model, source/format, token subdivisions and frozen nullable cost. The envelope carries producer UUID, sequence range and schema version. Request correlation can stay local; routing-profile snapshots, URLs, HTTP metadata, bodies, headers, chunks, credentials and databases are never in the batch. Aggregate these facts centrally after receipt, not into coarse time buckets before upload.

For batch-eligible ordinary inference, replace its per-request remote observation writes with the compact local capture; otherwise the metadata and most round trips would remain. That mode does not create a central request history for those operations. Video and other features that need a durable task identity keep their existing path. Use a dedicated central usage-facts projection rather than inventing request history to satisfy today's `usage_logs.request_id`; admin usage and quota reports must read both sources, without overlapping an event across them.

On startup reopen the same outbox and resume unacknowledged batches before scheduling the next upload. Seal one immutable, contiguous sequence range and payload digest at a time. Bound batch count and serialized bytes; periodic upload sends only pending facts. Validate a candidate five-minute interval against actual volume and a small bounded batch (for example 256 facts / 256 KiB before compression). Reserve enough local spool space before dispatch; a full spool rejects new admission instead of dropping executed usage. A disk failure after upstream execution surfaces the existing executed-accounting failure; it cannot be repaired by calling the provider again.

Reuse the existing private allowlisted network boundary. The current owner RPC re-authenticates the original user, so the autonomous receiver needs separate node-scoped credentials bound to the producer UUID; it must not reuse or persist a user Key. The receiver records unique event IDs, the batch receipt `(producerUUID, sequence range, payload digest)`, and usage projection in **one PG commit**. ACK only after commit. An identical retry returns the same receipt without adding usage; a reused sequence range with different content is rejected. Lost ACK retries exactly that batch. Mark it acknowledged or delete its local rows only after validating the receipt. Central quota reads committed facts for this protocol, not a second pending projection of the same fact.

### Quota visibility must be chosen explicitly

The management usage view can show `committedThrough` plus the node's unacknowledged count/oldest age. A five-minute delivery interval means centralized totals are delayed by that interval **plus outages**. Capture timestamps, not upload time, determine windows. Unknown cost stays NULL; corrections must be new identified facts, not repricing on retry.

Delayed reporting does not preserve the current global quota decision by itself. A node can see its own local unacknowledged usage, but not the other node's. There is no finite token/cost overshoot bound without bounds on request arrivals and maximum actual consumption. Do not call this a hard global limit.

Recommended first scope: batch only Keys with no configured quota; retain the current durable authoritative path for quota-controlled Keys until delayed quota enforcement is explicitly accepted. Before adding a quota to a previously batched Key, fence its admission on every node and flush all its pre-change facts; otherwise all-time/windowed quota omits earlier usage. This transition requires a new configuration contract, not a hidden fallback. If the user prefers all Keys to use delayed limits, document that decision and replace the quota reader/UI together; do not add prediction reservations or balance leases silently.

Fresh Key/account/group/model authorization remains online even with delayed statistics. Removing those WAN reads would require a separate revocation/config consistency decision. Async task identity/owner state also stays durable before its current public acknowledgement.

### Drain and replace a GCP node

Normal drain fences admission, joins active provider work, commits every captured fact locally, and forces a final upload immediately rather than waiting for the periodic timer. A successful fully-synchronized drain requires every sealed batch ACK and central reconciliation; it reports local unacknowledged count separately from PG pending.

If the WAN is unavailable, a **transfer-ready** state may replace fully-synchronized drain only under a new explicit carrier contract: freeze the outbox and producer, checkpoint/close SQLite, export a consistent SQLite backup plus producer UUID, sequence/ACK manifest and integrity identity, and verify import on the replacement before stopping the sole old owner. Never copy only the SQLite main file while its WAL is live. The target keeps the original producer/event/batch IDs, resends unacknowledged facts, and processes no request until single ownership is transferred. It must not generate fresh IDs for imported usage. Loss of an old VM before an off-machine copy of unacknowledged usage remains a data-loss risk; choose the existing protected backup destination and tolerated unsynced interval before enabling this mode.

Old-reader rollback still requires central ingestion of all local facts and zero legacy PG pending against the latest PG. A parked/exported outbox is not an empty accounting system. Axon authoritative PG/Redis stay on lag; Sub2's latest database/OAuth/Redis/appdata migration remains the separate Task46 contract.

Before approving this larger implementation, verify one SQLite fsync/reopen replay, a real-PG duplicate batch with commit ACK loss, cross-window/null-cost aggregation, a quota-mode transition, and a frozen export/import to a new node with the old writer fenced. Compare actual bytes/requests with the measured per-event SQL path. These are the missing checks for that proposal, not completed batch-feature evidence.

## Candidate verification

- Red: `TestClusterUsageRetryDoesNotRepublishAfterCommit` on the prior product, real PG18, returned **40 instead of 20 tokens** while the second handoff was parked. Both initial-publication ACK-known and ACK-lost/canceled cases reproduced it (`bash-27h9o2a3`).
- Green: both cases now expose exactly one 20-token fact from an independent quota pool throughout retry, zero revived pending, one final usage, unchanged UUID/time/tokens/NULL cost; startup recovery is idempotent. Existing real-PG two-pool snapshot, windows/null cost, additive migration/old-reader reconciliation tests also pass (`bash-fju9lqf7`).
- Real PG18 plus synthetic cold WAN: `TestClusterUsageColdWANBoundedDrain`, 8s configured only in the test, **4.458s**, one publication, three connections, usage=1/pending=0 (`bash-64cs3eue`).
- HTTP-to-stream pipeline regression: `TestObservationAccountingLostCommitACKDoesNotReplay`, transformed and pass-through Chat SSE, a real local SQLite commit followed by injected lost ACK, provider=1, publication=1, usage=1/pending=0. Existing final-usage, slow-publication error, Messages terminal and EOF/cancellation tests pass (`bash-64cs3eue`, `bash-p2o431fh`).

Only a disposable local PG18 container with synthetic data was used; no production image was built or published, and production env/services/settings were not changed.
