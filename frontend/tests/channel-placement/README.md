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

The fixture owns port `5195`; stop only the Vite process started for this fixture when finished. Its Vite cache is `.runtime/channel-placement-vite-cache`.

## Synthetic contract

The Vite middleware answers only local requests:

- `ChannelExecutionInventory` returns `clusterEnabled: true` and `node-alpha`, `node-beta`, `node-gamma`.
- `QueryChannels` returns 96 synthetic rows with 20-row pagination, interleaved machines, unassigned rows (`executionMachine: null`), enabled/disabled/archived status values, names, and models.
- The mock applies `input.executionMachine`, `input.where`, `input.model`, `input.hasTag`, and cursor pagination. It records the exact operation and variables in memory.
- Supporting read-only page queries (`CountChannelsByType`, `GetErrorChannelsCount`, `AllChannelTags`, `GetChannelProbeData`, `Models`, and `Me`) are synthetic too.
- The browser fetch guard returns the checked-in provider catalog for the production provider-data request and rejects every other external origin. No active backend or dev server is used.

Request evidence is available while the fixture is running at `GET /__channel_placement/requests`. `DELETE` clears the in-memory log.

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
6. Save a desktop screenshot and the request/screen evidence under `.runtime/channel-placement/` (ignored by the worktree), not in this fixture directory.
