# Channel execution placement browser fixture

This is an isolated Vite page for the read-only channel execution-machine milestone. It imports the production default export from `@/features/channels`, production CSS/components, the production `usePermissions` hook, and the production React providers needed by the page. It does not import the application route tree or connect to an AxonHub backend.

## Run

From the repository root:

```bash
pnpm --dir frontend exec vite --config tests/channel-placement/vite.config.ts
```

Open one of these loopback URLs:

- English: `http://127.0.0.1:5195/tests/channel-placement/?lang=en`
- Chinese: `http://127.0.0.1:5195/tests/channel-placement/?lang=zh`
- Read-only role: `http://127.0.0.1:5195/tests/channel-placement/?lang=en&role=readonly`
- System admin role: `http://127.0.0.1:5195/tests/channel-placement/?lang=en&role=admin`
- System owner role: `http://127.0.0.1:5195/tests/channel-placement/?lang=en&role=owner`

The fixture owns port `5195`; stop only the Vite process started for this fixture when finished. Its Vite cache is `.runtime/channel-placement-vite-cache`.

## Synthetic contract

The Vite middleware answers only local requests:

- `ChannelExecutionInventory` returns `clusterEnabled: true` and `node-alpha`, `node-beta`, `node-gamma`.
- `QueryChannels` returns 97 synthetic rows with 20-row pagination, interleaved machines, unassigned rows (`executionMachine: null`), enabled/disabled/archived status values, names, and models. The newest row is `Placement unassigned disabled`, a disabled channel with no execution machine and no existing credential group.
- The mock applies `input.executionMachine`, `input.where`, `input.model`, `input.hasTag`, and cursor pagination. It records the exact operation and variables in memory.
- Supporting page queries (`CountChannelsByType`, `GetErrorChannelsCount`, `AllChannelTags`, `GetChannelProbeData`, `Models`, `SystemChannelSettings`, `ProxyPresets`, `QueryChannelNames`, and `Me`) are synthetic too.
- `GET /admin/cluster/placement/:id`, `GET /admin/cluster/placement/:id/preview?target=xxx`, and `POST /admin/cluster/placement` implement the requested local observation/preview/save contract. Registered rows start at version 7 and return a two-channel shared-credential group with one disabled peer; the first-registration row returns `from: ""` and only itself in `channels`, then returns a Worker acknowledgement on a later observation GET.
- `role=readonly` has only read scopes and renders no placement action. `role=admin` has system-level `write_channels`; `role=owner` also sets `isOwner`.
- The browser fetch guard returns the checked-in provider catalog for the production provider-data request and rejects every other external origin. No active backend or dev server is used.

Request evidence is available while the fixture is running at `GET /__channel_placement/requests`; it includes both GraphQL and REST logs. `DELETE` resets all synthetic logs, placement state, and channel owners.

## Structure check

This check reads the production source and fixture files without changing them. It confirms the page/query contract and loopback-only fixture wiring:

```bash
node frontend/tests/channel-placement/check-structure.mjs
```

## Browser acceptance sequence

Use visible text and the stable production test IDs:

1. Confirm the initial table has 20 rows, the machine selector exposes `All machines` / `全部机器`, and the execution-machine cells include both real node IDs and the unassigned label.
2. Select `node-alpha` (or another real node) from `data-testid="channel-execution-machine-filter"`; verify the table narrows and the recorded `QueryChannels.variables.input.executionMachine` is that exact node ID.
3. Move to the next page; verify the request carries the returned `after` cursor and still carries the selected node ID.
4. Narrow by status, then name, then model using the real toolbar controls; verify each request records the corresponding server-side filter and the visible rows shrink.
5. Reset filters; verify the machine selector returns to `All machines`, the first page returns, `executionMachine` is omitted from the input, and `paginateModelMatches: true` remains present.
6. Open `role=readonly`; verify rows still show execution machines but no `channel-placement-change` action exists.
7. Open `role=admin`; choose a visible source-machine row with `data-testid="channel-placement-change"`. Verify `channel-placement-dialog`, `placement-current-owner`, `placement-target`, and `placement-impact`; the impact list contains exactly two shared-group rows, including disabled/archived status, and the online-help copy says existing requests finish while other channels stay available.
8. Choose another machine and save with `placement-save`. Verify the table refetch removes the old-owner row while the dialog remains open, `placement-sync-status` shows ownership saved, target applied, and Worker pending; after the next observation GET it shows Worker synchronized. Close the dialog, filter to the target machine, and verify the moved rows appear there.
9. For first registration, open the newest `Placement unassigned disabled` row after the product action is available. Verify the dialog current machine is `Unassigned`, choose a real node, and confirm preview `from` is empty with exactly that disabled row in `channels`.
10. Save the first registration, verify the target-filtered table shows the new node owner and the row remains Disabled. Save a desktop screenshot and the request/screen evidence under `.runtime/channel-placement/` (ignored by the worktree), not in this fixture directory.
