import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import ts from 'typescript';

const srcRoot = join(import.meta.dirname, '..', '..');
const read = (relativePath) => readFileSync(join(srcRoot, relativePath), 'utf8');

const workflowSource = read('features/normal-user-portal/workflow.ts');
const workflowModuleUrl = `data:text/javascript;base64,${Buffer.from(
  ts.transpileModule(workflowSource, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2023 },
  }).outputText
).toString('base64')}`;
const workflow = await import(workflowModuleUrl);
const classificationPreviewSource = read('features/apikeys/components/classification-preview.ts');
const classificationPreviewModuleUrl = `data:text/javascript;base64,${Buffer.from(
  ts.transpileModule(classificationPreviewSource, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2023 },
  }).outputText
).toString('base64')}`;
const classificationPreview = await import(classificationPreviewModuleUrl);

test('URL handoff validation fails closed and compatible model selection respects the Access Group', () => {
  assert.deepEqual(workflow.validateSelfServiceHandoff({ accessGroupId: '-2', modelId: 7 }), {});
  assert.deepEqual(workflow.validateSelfServiceHandoff({ accessGroupId: '12', modelId: ' model-b ' }), {
    accessGroupId: 12,
    modelId: 'model-b',
  });

  const models = [
    { id: 'model-a', name: 'A', accessGroups: [{ id: 12, profileId: 12 }] },
    { id: 'model-b', name: 'B', accessGroups: [{ id: 15, profileId: 15 }] },
    { id: 'model-c', name: 'C', presetId: 12 },
  ];
  assert.equal(workflow.selectCompatibleModel(models, { accessGroupId: 12, modelId: 'model-b' }), undefined);
  assert.equal(workflow.selectCompatibleModel(models, { accessGroupId: 12, modelId: 'model-c' }).id, 'model-c');
});

test('secret reducer keeps values in component state and clears only the affected lifecycle', () => {
  const secret = { keyId: 9, keyName: 'local', value: 'sk-owned', accessGroupId: 12 };
  assert.deepEqual(workflow.reduceRevealedSecret(null, { type: 'revealed', secret }), secret);
  assert.deepEqual(workflow.reduceRevealedSecret(secret, { type: 'key-transition', keyId: 8 }), secret);
  assert.equal(workflow.reduceRevealedSecret(secret, { type: 'key-transition', keyId: 9 }), null);
  assert.equal(workflow.reduceRevealedSecret(secret, { type: 'clear' }), null);
  assert.equal(workflow.secretSupportsAccessGroup(secret, 12), true);
  assert.equal(workflow.secretSupportsAccessGroup(secret, 15), false);
});

test('self-service provenance classification never guesses legacy ownership', () => {
  assert.equal(workflow.classifySelfAPIKey({ provisioningSource: 'legacy_unknown', profileMode: 'snapshot' }), 'legacy_unknown');
  assert.equal(
    workflow.classifySelfAPIKey({ provisioningSource: 'self_service', profileMode: 'access_group' }),
    'self_service_access_group'
  );
  assert.equal(workflow.classifySelfAPIKey({ provisioningSource: 'self_service', profileMode: 'snapshot' }), 'self_service_snapshot');
  assert.equal(workflow.keySupportsAccessGroup({ profileMode: 'access_group', accessGroupId: 12 }, 12), true);
  assert.equal(workflow.keySupportsAccessGroup({ profileMode: 'snapshot', accessGroupId: 12 }, 12), false);
});

test('request evidence marks unavailable persisted fields as partial', () => {
  const available = { state: 'available', source: 'database' };
  assert.equal(
    workflow.isPartialEvidence({
      requestHeaders: available,
      requestBody: available,
      responseBody: available,
      responseChunks: { state: 'notApplicable', source: 'none' },
    }),
    false
  );
  assert.equal(
    workflow.isPartialEvidence({
      requestHeaders: available,
      requestBody: { state: 'storageUnavailable', source: 'external' },
      responseBody: available,
      responseChunks: available,
    }),
    true
  );
});

test('write controls are omitted before rendering for read-only administrators', () => {
  const primary = read('features/apikeys/components/apikeys-primary-buttons.tsx');
  const dialogs = read('features/apikeys/components/apikeys-dialogs.tsx');
  const accessGroups = read('features/access-groups/index.tsx');
  const rowActions = read('features/apikeys/components/data-table-row-actions.tsx');

  assert.match(primary, /if \(!canWrite\) return null/);
  assert.match(dialogs, /\{canWrite && \(/);
  assert.match(accessGroups, /canReadPolicy = apiKeyPermissions\.canRead && channelPermissions\.canRead/);
  assert.match(accessGroups, /canEditPolicy = canReadPolicy && apiKeyPermissions\.canWrite/);
  assert.match(accessGroups, /if \(!canEditPolicy\) throw new Error/);
  assert.match(accessGroups, /disabled=\{!canEditPolicy \|\| assignChannels\.isPending\}/);
  assert.match(accessGroups, /\{!canReadPolicy && \(/);
  assert.match(rowActions, /apiKeyPermissions\.canWrite && \(/);
});

test('Access Group policy queries fail closed before issuing model or channel requests', () => {
  const accessGroups = read('features/access-groups/index.tsx');
  const adminClassification = read('features/apikeys/components/apikeys-classification-dialog.tsx');
  assert.match(accessGroups, /enabled: Boolean\(projectId\) && canReadPolicy/);
  assert.match(accessGroups, /if \(canReadPolicy\) return;[\s\S]*setSelectedChannelIds\(\[\]\)/);
  assert.match(adminClassification, /canReadAccessGroupPolicy = apiKeyPermissions\.canRead && channelPermissions\.canRead/);
  assert.match(adminClassification, /enabled: open && mode === 'personal_access_group' && Boolean\(projectId\) && canReadAccessGroupPolicy/);
});

test('administrator legacy classification uses only backend-supported modes and previews every policy dimension', () => {
  const client = read('lib/api-client.ts');
  const dialog = read('features/apikeys/components/apikeys-classification-dialog.tsx');
  assert.doesNotMatch(client + dialog, new RegExp(`admin_${'managed'}`));
  for (const mode of ["'admin'", "'personal_snapshot'", "'personal_access_group'"]) {
    assert.match(client + dialog, new RegExp(mode));
  }
  for (const dimension of ['models', 'channels', 'routing', 'quota']) {
    assert.match(dialog, new RegExp(`classification\\.preview\\.${dimension}`));
  }
  assert.match(dialog, /channelPermissions\.canRead/);
});

test('invalid semantic handoffs are scrubbed with replace navigation instead of preserving fallback URLs', () => {
  const portal = read('features/normal-user-portal/index.tsx');
  assert.match(portal, /setHandoffSelectionRequired\(true\)/);
  assert.match(portal, /search: \{ accessGroupId: handoff\.accessGroupId \}/);
  assert.match(portal, /replace: true/);
  assert.match(portal, /handoffSelectionRequired\s*\?\s*undefined/);
});

test('revealed secrets remain bound to their Access Group before entering a snippet', () => {
  const portal = read('features/normal-user-portal/index.tsx');
  assert.match(portal, /secretSupportsAccessGroup\(revealedSecret,/);
  assert.match(portal, /accessGroupId: key\.accessGroupId/);
  assert.match(portal, /dispatchRevealedSecret\(\{ type: 'clear' \}\)/);
});

test('creator Access Group classification uses visible policy data and the shared comparison component', () => {
  const portal = read('features/normal-user-portal/index.tsx');
  assert.match(portal, /selfServiceApi\.accessGroups\(projectID\)/);
  assert.match(portal, /classificationTargetProfile\?\.modelIds/);
  assert.match(portal, /formatPolicyQuota\(key\.quotaSummary, t\)/);
  assert.match(portal, /PolicyComparisonPreview rows=\{creatorClassificationPreview\}/);
  assert.match(portal, /liveGroupConstraints/);
  assert.match(portal, /liveGroupRouting/);
});

test('shared quota formatter emits localized English and Chinese units', () => {
  const locales = {
    en: JSON.parse(read('locales/en/apikeys.json')),
    zh: JSON.parse(read('locales/zh-CN/apikeys.json')),
  };
  const translator = (locale) => (key, values = {}) => {
    const template = locales[locale][key] ?? key;
    return Object.entries(values).reduce((value, [name, replacement]) => value.replaceAll(`{{${name}}}`, String(replacement)), template);
  };
  const quota = { requests: 2, totalTokens: 1000, cost: '3.5', period: 'all_time' };
  assert.equal(classificationPreview.formatPolicyQuota(quota, translator('en')), '2 requests · 1,000 tokens · Cost 3.5 · All time');
  assert.equal(classificationPreview.formatPolicyQuota(quota, translator('zh')), '2 次请求 · 1,000 个令牌 · 费用 3.5 · 不限周期');
  assert.doesNotMatch(classificationPreviewSource, /`\$\{quota\.(?:requests|totalTokens)\} (?:requests|tokens)`/);
});

test('reveal, destructive confirmation, and memory-clearing contracts are wired', () => {
  const portal = read('features/normal-user-portal/index.tsx');
  assert.match(portal, /selfServiceApi\.revealAPIKey\(key\.id\)/);
  assert.match(portal, /dispatchRevealedSecret\(\{ type: 'clear' \}\)/);
  assert.doesNotMatch(portal, /localStorage|sessionStorage/);
  assert.match(portal, /setConfirmedKeyAction\(\{ type: 'rotate', key \}\)/);
  assert.match(portal, /setConfirmedKeyAction\(\{ type: 'archive', key \}\)/);
  assert.doesNotMatch(portal, /onClick=\{\(\) => rotateKey\.mutate\(key\)\}/);
});

test('project Access Group route and URL handoff replace the global-name workflow', () => {
  const accessGroups = read('features/access-groups/index.tsx');
  const sidebar = read('sidebar.ts');
  const oldRoute = read('routes/_authenticated/access-groups/index.tsx');
  const keyRoute = read('routes/_authenticated/self-service/api-keys/index.tsx');
  const portal = read('features/normal-user-portal/index.tsx');

  assert.doesNotMatch(accessGroups, /selfServicePresetNames|updateAdminRegistrationPolicy|WILDCARD_PRESET/);
  assert.match(sidebar, /url: '\/project\/access-groups'/);
  assert.match(oldRoute, /redirect\(\{ to: '\/project\/access-groups', replace: true \}\)/);
  assert.match(keyRoute, /validateSearch: validateSelfServiceHandoff/);
  assert.match(portal, /search: \{ accessGroupId: presetID, modelId \}/);
});

test('request details expose disabled, empty, partial, error, and loaded states with locale parity', () => {
  const portal = read('features/normal-user-portal/index.tsx');
  for (const marker of ['detailDisabledError', 'detailEmpty', 'detailPartial', 'detailError', 'detailComplete']) {
    assert.match(portal, new RegExp(`selfService\\.requests\\.${marker}`));
  }

  const en = JSON.parse(read('locales/en/selfService.json'));
  const zh = JSON.parse(read('locales/zh-CN/selfService.json'));
  assert.deepEqual(Object.keys(en).sort(), Object.keys(zh).sort());
});

test('all touched locale bundles keep English and Chinese key parity', () => {
  for (const file of ['accessGroups.json', 'apikeys.json', 'selfService.json']) {
    const en = JSON.parse(read(`locales/en/${file}`));
    const zh = JSON.parse(read(`locales/zh-CN/${file}`));
    assert.deepEqual(Object.keys(en).sort(), Object.keys(zh).sort(), `${file} locale keys should match`);
  }
});
