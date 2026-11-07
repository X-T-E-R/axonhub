import { readFile } from 'node:fs/promises';
import path from 'node:path';

const fixtureDir = path.dirname(new URL(import.meta.url).pathname).replace(/^\/(?=[A-Za-z]:)/, '');
const repoRoot = path.resolve(fixtureDir, '../../..');

const files = {
  page: path.join(repoRoot, 'frontend/src/features/channels/index.tsx'),
  query: path.join(repoRoot, 'frontend/src/features/channels/data/channels.ts'),
  toolbar: path.join(repoRoot, 'frontend/src/features/channels/components/data-table-toolbar.tsx'),
  columns: path.join(repoRoot, 'frontend/src/features/channels/components/channels-columns.tsx'),
  placementCell: path.join(repoRoot, 'frontend/src/features/channels/components/channel-execution-machine-cell.tsx'),
  placementData: path.join(repoRoot, 'frontend/src/features/channels/data/placement.ts'),
  main: path.join(fixtureDir, 'main.tsx'),
  mock: path.join(fixtureDir, 'mock-server.ts'),
  vite: path.join(fixtureDir, 'vite.config.ts'),
};

const source = {};
for (const [name, file] of Object.entries(files)) {
  source[name] = await readFile(file, 'utf8');
}

const checks = [
  ['production page imports the execution inventory hook', source.page.includes('useChannelExecutionInventory')],
  ['production page sends paginateModelMatches', source.page.includes('paginateModelMatches: true')],
  ['production page sends the optional execution machine', source.page.includes('executionMachine: executionMachine || undefined')],
  ['QueryChannels selects executionMachine', source.query.includes('executionMachine\n')],
  ['QueryChannels supports paginateModelMatches', source.query.includes('paginateModelMatches?: boolean')],
  ['inventory operation name is stable', source.query.includes('query ChannelExecutionInventory')],
  ['machine filter test id is stable', source.toolbar.includes("data-testid='channel-execution-machine-filter'")],
  ['machine cell test id is stable in the extracted production cell', source.placementCell.includes("data-testid='channel-execution-machine'")],
  ['placement action test id is stable', source.placementCell.includes("data-testid='channel-placement-change'")],
  ['placement dialog test ids are present', ['channel-placement-dialog', 'placement-current-owner', 'placement-target', 'placement-impact', 'placement-save', 'placement-sync-status'].every((id) => source.placementCell.includes(`data-testid='${id}'`))],
  ['placement REST client uses the requested endpoint', source.placementData.includes("const endpoint = '/admin/cluster/placement'")],
  ['fixture renders the production default export', source.main.includes("import ChannelsManagement from '@/features/channels'")],
  ['fixture supports privileged and read-only roles', source.main.includes("requestedRole === 'admin' || requestedRole === 'owner'") && source.mock.includes('isPrivilegedRequest')],
  ['fixture implements synthetic placement REST', source.mock.includes('handlePlacementRequest') && source.vite.includes('handlePlacementRequest')],
  ['fixture includes a disabled first-registration row', source.mock.includes('Placement unassigned disabled') && source.mock.includes("status: 'disabled'") && source.mock.includes('executionMachine: null')],
  ['fixture has an unassigned placement branch', source.mock.includes("return { owner: '', channelIDs: [channelID] }")],
  ['fixture has a loopback-only Vite server', source.vite.includes("host: '127.0.0.1'") && source.vite.includes('port: 5195')],
  ['fixture keeps Vite cache under ignored .runtime', source.vite.includes('.runtime/channel-placement-vite-cache')],
  ['fixture does not point at the managed frontend or backend ports', !source.vite.includes('5173') && !source.vite.includes('8090')],
];

const failures = checks.filter(([, passed]) => !passed).map(([name]) => name);
if (failures.length > 0) {
  console.error('Channel-placement fixture structure check failed:');
  for (const failure of failures) console.error(`- ${failure}`);
  process.exitCode = 1;
} else {
  console.log(`Channel-placement fixture structure check passed (${checks.length} assertions).`);
}
