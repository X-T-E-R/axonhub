# Task49 accounting: confirmed publication, WAN recovery and batch proposal

## Local candidate and release consumption

This candidate separates optional observation failures from required accounting. Optional request/execution metadata and terminals use the existing bounded retry budget; frozen usage stays owned until handoff or shutdown. A scope first reserves its usage credit, so missing history capacity need not reject ordinary inference. If the required credit is unavailable, `CreateRequest` rejects before provider dispatch, including for Keys without quotas. Video task ID/channel correlation remains required business state and retains its accepted jobs and lifecycle admission.

The source tree is `E:/kiki-wt/axon-multi-node-edge-20261104`, based on `c0db180c`. That earlier ACK fix is already consumed as main `3c0cb577` and isolated f6cd release `14759fb2`, but has not been loaded in production. Consume this increment onto the isolated release source, not the root branch containing paused raw work. It adds no schema, outbox, node-auth protocol, default timeout, provider setting or production resource change.

## Atomic accounting and failure recovery

Each cluster capture publishes a UUID-keyed compact `pending_usages` row before queueing its usage job. Handoff locks that row, creates `usage_logs` and deletes pending in one `RunInTransaction` commit. Quota reads both tables in one repeatable-read snapshot. Once publication is acknowledged, retries perform **handoff only**: a missing pending row means the fact was consumed, not that it should be republished. An unacknowledged publication retries the same UUID, capture time, tokens and nullable cost.

Optional `core`, `metadata` and `terminal` jobs now stop after `MaxAttempts`; `errObservationParentUnavailable` was already an immediate exit and remains one. Required usage has no attempt-count cap. A lost optional parent is recovered by its observation nonce or a compact skeleton in the usage transaction, in both cluster and standalone writer paths. The skeleton retains project/Key/channel ownership and unavailable evidence; its status stays `processing` because usage can accompany cancellation or failure and does not prove a successful terminal.

A failed capture-policy read explicitly disables request body, execution request body, response body, chunks and live preview in the local snapshot. Optional jobs cannot fall through to enabled defaults. Last verified capacity settings remain available for managed usage bookkeeping; the usage handoff does not first read optional policy over the WAN. Configured quotas still read authoritative usage/pending and fail on an actual quota-read error. A missing setting retains the legacy defaults. Startup can proceed with optional capture disabled, but must still reconcile the node's durable pending successfully before admission.

If publication fails or its ACK is unknown, the response reports an `ExecutedAccountingError` and the required writer retains the frozen fact for recovery. The type fence precedes transport timeout/status patterns, alternate-model retries and failure-policy routing. It also prevents circuit-breaker failures, provider cooldowns and failed performance records from being manufactured by a gateway accounting error. HTTP/SSE/WS keep the existing executed/unknown, final-usage, cancellation and semantic-completion contracts. Only one process may own a node ID; recovery neither reprices nor expires its facts.

## Incident cause and memory evidence

The reproduced product faults are optional FIFO starvation, dispatch without a usage reservation, and accounting errors entering provider failure classifiers. The supplied incident evidence separately establishes lag's repeated `CONSTRAINT_MEMCG` OOM kills. The later upgrade 502 samples occurred before `response.create`; they do not show a replay or a model execution. Policy warnings alone do not establish that large bodies reached PG or caused the OOM.

Observation contexts already detach the pipeline container. Usage jobs now retain only explicit accounting identity; acknowledged PG facts queue only handoff IDs. Completed external saves release their body while any compact disposition patch waits. Core and payload lanes each retain their configured bounded budget; stream observations charge eight times their source event size within the core budget. Those budgets exclude active pipeline/business buffers and Go/runtime overhead, so they are not a total RSS limit.

In the current three-concurrent-30-MiB capture test, capture disabled allocated **19,152 B**, with **4,304 B** live heap delta and **70,898 B** queue charge. Capture enabled allocated **62,950,800 B**, with **62,931,720 B** live heap delta and **62,985,482 B** charge: two canonical bodies fit the 64-MiB budget, while the third degrades to metadata. The 20,000-event disabled stream test allocated **0 B** and retained no chunks. These synthetic observations establish the capture boundary; production OOM attribution still needs process heap/active-stream evidence after loading the fix.

## What crosses the WAN today

The five shared capture switches being false removes ordinary inference request/response bodies, headers, chunks and live preview. It does not make the product a stats-only protocol.

| Path | Data and dependency | Source |
| --- | --- | --- |
| Synchronous usage capture | UUID, node, Key/project/channel IDs, actual model, source/format, capture/request time, request ID/observation nonce, token details, frozen nullable cost/items/price reference. No payloads or credentials in this row. | `biz/pending_usage.go:18–34` |
| Writer FIFO | Optional metadata and evidence still use fresh storage policy and request/execution inserts or terminal updates. Request metadata includes model, stream/source, Key/project/trace, client IP and routing-profile snapshot. Execution metadata includes channel, actual model, request URL, masked selected-key label, metrics and status. Acknowledged usage queues only IDs; its PG handoff uses the verified capacity snapshot. | `biz/forwarding_observation.go` (`run`), `biz/usage_log.go` (`CreateUsageLogFromRequest`) |
| Fresh authorization | Key/project lookup plus owner and membership checks for managed personal Keys; HTTP and each WS generation remain dependent on authoritative authorization. | `biz/auth.go:218`, `biz/portal_access.go:20–66` |
| Quota | Configured quotas perform a primary-PG repeatable-read aggregation of final and pending facts. No-quota admission skips this query, but still durably captures real usage. | `biz/quota.go:52–76`, `biz/pending_quota.go` |
| Other shared state | Channel/config/price loading, account/OAuth changes, async task records and shared Redis cache/watcher/limiter operations remain cross-node dependencies. A stats batching change alone does not remove them. | `biz/channel.go:229–295`, `biz/task_execution.go`, `cluster/config.go` |

The same local **PostgreSQL 18.0** fixture uses two pools, zero writer idle connections and a synthetic 140ms delay before each TCP protocol write plus dial. For one 17+3-token capture, handoff and stop, removing the usage policy query changed **4.458s to 2.890s**, **three cold connections to two**, and **6,203 B sent / 3,638 B received to 5,123 B / 2,443 B**. These are PG protocol bytes, not Ethernet/WireGuard traffic. The sample excludes request/execution creation, authentication and Redis; it measures cold SQL round trips, not production throughput or memory.

## Loading a recovery timeout or fixed image

`AXONHUB_MANAGED_REQUEST_BODY_WRITER_ATTEMPT_TIMEOUT=8s` is an existing **startup-only mitigation candidate**, not the low-frequency architecture. The candidate keeps the 2s default. The local 8s test finishes an entire healthy cold-WAN capture and drain within that budget; it does not promise bounded draining through a database outage. Raising this value also raises the existing retry pause, because both use the same setting.

For the current f6cd process, `/drain` has a hard-coded 900s lifetime. On expiry `writer.Stop` cancels the worker and can abandon RAM lifecycle jobs, although durable PG pending survives. The CLI has no deadline argument, and extending the SSH/CLI timeout cannot change this. Keep an acknowledged maintenance pause while the writer makes progress; do not use pending=0 or activeHTTP=0 as a substitute for a drained writer. The deferred Sub2 carrier may retain that paused process and postpone Axon finalization; it must report `writersStopped:false` honestly.

An env change requires Compose recreate; `docker start` reuses the old env. A product fix requires a new image from the isolated **f6cd plus both accounting increments**. This local candidate leaves the live 1 GiB memory / 2 GiB memory-plus-swap stopgap in place; no image has been built or loaded. Validate real connection/heap behavior after an authorized load before judging whether to change the runtime limit. Sub2 cutover is independent of this release.

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

Fresh Key/account/group/model authorization remains online even with delayed statistics. Removing those WAN reads would require a separate revocation/config consistency decision. Keep async task ID and owner persistence on its business path; never defer it to the statistics upload interval.

### Drain and replace a GCP node

Normal drain fences admission, joins active provider work, commits every captured fact locally, and forces a final upload immediately rather than waiting for the periodic timer. A successful fully-synchronized drain requires every sealed batch ACK and central reconciliation; it reports local unacknowledged count separately from PG pending.

If the WAN is unavailable, a **transfer-ready** state may replace fully-synchronized drain only under a new explicit carrier contract: freeze the outbox and producer, checkpoint/close SQLite, export a consistent SQLite backup plus producer UUID, sequence/ACK manifest and integrity identity, and verify import on the replacement before stopping the sole old owner. Never copy only the SQLite main file while its WAL is live. The target keeps the original producer/event/batch IDs, resends unacknowledged facts, and processes no request until single ownership is transferred. It must not generate fresh IDs for imported usage. Loss of an old VM before an off-machine copy of unacknowledged usage remains a data-loss risk; choose the existing protected backup destination and tolerated unsynced interval before enabling this mode.

Old-reader rollback still requires central ingestion of all local facts and zero legacy PG pending against the latest PG. A parked/exported outbox is not an empty accounting system. Axon authoritative PG/Redis stay on lag; Sub2's latest database/OAuth/Redis/appdata migration remains the separate Task46 contract.

Before approving this larger implementation, verify one SQLite fsync/reopen replay, a real-PG duplicate batch with commit ACK loss, cross-window/null-cost aggregation, a quota-mode transition, and a frozen export/import to a new node with the old writer fenced. Compare actual bytes/requests with the measured per-event SQL path. These are the missing checks for that proposal, not completed batch-feature evidence.

## Candidate verification

- Red: `TestClusterUsageRetryDoesNotRepublishAfterCommit` on the prior product, real PG18, returned **40 instead of 20 tokens** while the second handoff was parked. Both initial-publication ACK-known and ACK-lost/canceled cases reproduced it (`bash-27h9o2a3`).
- PG18 recovery: both ACK cases expose one 20-token fact from an independent quota pool throughout retry, with zero revived pending, one final usage and unchanged timestamp/NULL cost. The same fixture checks a permanently failing optional core INSERT, a durable acknowledged fact larger than its 1 MiB queue budget, cold WAN and additive rollback (`bash-rgoucb71`, `bash-joa7df61`, `bash-3lgla5lf`).
- Writer failure and admission: policy-only timeout originally blocked startup and kept usage at the head of the FIFO beyond attempt 31 (`bash-q0idn49k`). The candidate passes policy-only failure, optional retry exhaustion, missing parent, required retry, quota and memory tests. A policy-only timeout plus a transient INSERT failure still preserves the video external ID and channel (`bash-r17dkzif`, `bash-w6sjiopc`).
- Protocol and health: saturated real Chat pipeline makes **zero** provider calls; one returned usage credit permits **one** call, correct SSE terminal and one usage row without an execution-history row. Existing transformed/pass-through, final usage, semantic completion, cancel/EOF and WS checks pass. Wrapped `DeadlineExceeded` originally retried and pushed the breaker to five failures/Break; now it produces zero provider failures and preserves a successful performance sample. An actual upstream 429 still cools down its channel (`bash-u36h0g2a`, `bash-cngc385l`, `bash-4vrmpha2`, `bash-3mghmfq4`).

The existing RAM reserve covers ordinary compact facts. Before initial PG acknowledgement, the byte gate still applies to larger provider usage/price breakdowns; a fact larger than the available budget cannot be treated as recoverable if its initial PG insert did not commit. The outbox proposal addresses crash durability; neither an ACK-less fact nor a process reporting `nodePending=0` alone is proof that it is safe to replace the writer.
