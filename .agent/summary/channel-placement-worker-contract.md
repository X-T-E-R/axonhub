# Channel placement and Worker consumption

Axon is the authority for the effective owner of a channel. The original **Channels** page applies online moves to an entire shared credential group. Worker consumes the versioned effective manifest asynchronously; the UI's preview is not routing input.

## Edge contract

Fetch authenticated `GET /admin/cluster/manifest` from the control node. It returns `Cache-Control: no-store` and contains no credentials, channel IDs, request bodies or Key IDs. `version: 1` is the existing wire format. `placement_version` is a monotonically increasing integer, omitted before the first online save. `generated_at` indicates when this projection was built, not when the edge applied it. Each model row is `{model, owner?, owners, status}`. Apply a single `ready` owner; retain existing `ambiguous` and `unavailable` handling and Key/catalog restrictions.

The deployment controller should fetch and apply the effective manifest outside inference traffic, then verify the installed revision and report `POST /admin/cluster/placement/worker-ack` using a system owner JWT:

```json
{"placement_version": 12, "worker_revision": "<read-back revision>"}
```

The server accepts the current version only. The same version and revision are idempotent. Never report a successful synchronization on an attempted deployment or a cached manifest. Worker consumes only the manifest and does not need the mutation credential. Preserve Task52's fixed DeepSeek exact-ID routes and Task49's Responses, compact and WebSocket passthrough; they are explicit edge exceptions, not inferred from a generic model owner.

## Axon control and observation

Cluster nodes load the effective shared-primary snapshot at startup and through the existing channel-cache notification and periodic refresh. Static `cluster.channels` supplies original bindings. The same snapshot now also holds optional `channel_identities` for channels first registered online; their opaque identity and group owner are committed together in the existing System row. Old snapshot values without this field remain readable. Deploy the compatible code to all nodes before using first registration; no schema migration or per-channel environment edit is needed.

- `GET /admin/cluster/placement/:id`: `read_channels`, returns `placement_version`, `owner`, and, after an assignment or move, the last `change`, `target` readback or `target_error`, and most recent `worker_ack`. An unassigned channel reports `owner: ""`. A successful target readback confirms the node has loaded the saved ownership and any affected enabled providers. Worker acknowledgement is separate.
- `GET /admin/cluster/placement/:id/preview?target=<node>`: system `write_channels`, returns the current version, source, target and affected channel IDs, names and statuses. First assignment requires a disabled channel and reports `from: ""`. If the credentials already have an owner, select that owner and confirm the group. Stable credential identities and raw values are omitted.
- `POST /admin/cluster/placement`: system `write_channels`, accepts `{channel_id,target,expected_version,request_id,channel_ids}` from a current preview. A fresh version and confirmed IDs protect newer edits and unconfirmed peers. Retrying the same committed request ID reads its result. Assignment preserves status; use the existing enable operation separately. Unassigned channels cannot be enabled or appear in the model manifest.

Target preparation constructs the enabled channels' providers without starting OAuth loops or sending upstream requests. A preparation failure leaves the owner unchanged. The save does not need to contact the source. After commit, the target refreshes and other nodes converge through the normal cache lifecycle. A placement-only refresh leaves other providers running and does not edit channel rows. In-flight requests finish on the provider they selected; old caches and OAuth cleanup can overlap briefly. If a save response is lost, read the selected channel's current owner and `change.request_id` before taking another action.

Read-only GraphQL remains additive: `channelExecutionInventory { clusterEnabled machines }`, nullable `Channel.executionMachine`, and `QueryChannelInput.executionMachine` / `paginateModelMatches`. `executionMachine` applies before pagination. Omitting `paginateModelMatches` preserves the legacy unpaginated model search when no machine is selected. The channels page opts into model pagination for all machines too.

## Rollback and integration

Undo a saved move by making a fresh change back to the previous owner, then synchronize the new Worker version. Before downgrading to static-only Axon, restore static owners and disable channels registered only online, then synchronize the Worker. Neither operation requires restoring the database or clearing usage.

Source worktree: `E:/kiki-wt/axon-channel-placement-control-20261107`, base `d9513ee6`, read-only commit `169b5fc2`. Release tree: use the no-raw line based on `e1765eb9` with read-only commit `5737a2c1`; regenerate GraphQL against that tree instead of copying `d9513ee6` generated output. The online slice itself does not change the GraphQL schema or add raw behavior.
