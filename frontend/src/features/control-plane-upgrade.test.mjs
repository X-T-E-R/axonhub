import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import test from 'node:test';
import ts from 'typescript';

const srcRoot = join(import.meta.dirname, '..');
const requireFromFrontend = createRequire(join(srcRoot, '..', 'package.json'));
const transpiledCache = new Map();

async function resolveModuleUrl(relativePath) {
  if (transpiledCache.has(relativePath)) {
    return transpiledCache.get(relativePath);
  }
  const source = readFileSync(join(srcRoot, relativePath), 'utf8');
  let output = ts.transpileModule(source, {
    compilerOptions: {
      module: ts.ModuleKind.ESNext,
      target: ts.ScriptTarget.ES2023,
    },
  }).outputText;

  const importMatches = [...output.matchAll(/from\s+['"]([^'"]+)['"]/g)];
  for (const match of importMatches) {
    const specifier = match[1];
    let resolvedHref;
    if (specifier.startsWith('@/')) {
      const subRel = `${specifier.slice(2)}.ts`;
      resolvedHref = await resolveModuleUrl(subRel);
    } else if (!specifier.startsWith('data:') && !specifier.startsWith('file:') && !specifier.startsWith('node:')) {
      resolvedHref = pathToFileURL(requireFromFrontend.resolve(specifier)).href;
    }
    if (resolvedHref) {
      output = output.replaceAll(`'${specifier}'`, `'${resolvedHref}'`).replaceAll(`"${specifier}"`, `"${resolvedHref}"`);
    }
  }

  const dataUrl = `data:text/javascript;base64,${Buffer.from(output).toString('base64')}`;
  transpiledCache.set(relativePath, dataUrl);
  return dataUrl;
}

async function importTypeScript(relativePath) {
  const url = await resolveModuleUrl(relativePath);
  return import(url);
}

function loadLocaleBundle(localeDir) {
  const dir = join(srcRoot, 'locales', localeDir);
  const merged = {};
  for (const file of readdirSync(dir)) {
    if (!file.endsWith('.json')) continue;
    Object.assign(merged, JSON.parse(readFileSync(join(dir, file), 'utf8')));
  }
  return merged;
}

const merge = await importTypeScript('features/channels/utils/merge.ts');
const channelSchema = await importTypeScript('features/channels/data/schema.ts');
const apiKeySchema = await importTypeScript('features/apikeys/data/schema.ts');

test('mergeChannelSettingsForUpdate losslessly preserves credentialPolicies, balanceProbe.script, and sseKeepAlive', () => {
  const existing = {
    extraHeaders: { 'X-Trace': '1' },
    credentialPolicies: [
      {
        id: 'cred_fp_1',
        name: 'Primary Codex Account',
        priority: 0,
        weight: 200,
        maxConcurrent: 4,
        requestIntervalMs: 250,
        manualQuotaLimit: 1000,
        egressMode: 'pinned_proxy',
        outboundProxyId: 12,
        proxyPoolName: null,
        customProxy: null,
        egressFailPolicy: 'backup_proxy',
      },
      {
        id: 'cred_fp_2',
        name: 'Pool Account',
        priority: 1,
        weight: 100,
        egressMode: 'proxy_pool',
        proxyPoolName: 'us-resi',
        egressFailPolicy: 'pool_fallback',
      },
    ],
    balanceProbe: {
      enabled: true,
      preset: 'custom',
      baseURL: 'https://api.example.com',
      path: '/v1/usage',
      method: 'GET',
      expectedStatuses: [200],
      includeStatuses: ['active', 'disabled'],
      keyInjection: 'authorization_bearer',
      primarySelection: 'auto_highest',
      timeoutMs: 8000,
      script: {
        mode: 'expr',
        expression: '{ remaining: body.credits_left, limit: body.credits_total }',
        command: [],
      },
    },
    sseKeepAlive: {
      enabled: true,
      intervalSeconds: 15,
    },
    keyHealth: {
      keys: [{ id: 'cred_fp_1', status: 'active' }],
    },
  };

  const merged = merge.mergeChannelSettingsForUpdate(existing, {
    extraModelPrefix: 'team/',
  });

  assert.equal(merged.extraModelPrefix, 'team/');
  assert.equal('keyHealth' in merged, false);
  assert.deepEqual(merged.credentialPolicies, existing.credentialPolicies);
  assert.deepEqual(merged.balanceProbe, existing.balanceProbe);
  assert.deepEqual(merged.sseKeepAlive, existing.sseKeepAlive);
});

test('channel schemas match frozen GraphQL enums for egress, failover, scheduling, and balance script modes', () => {
  assert.deepEqual(channelSchema.credentialEgressModeSchema.options, [
    'inherit_channel',
    'direct',
    'pinned_proxy',
    'proxy_pool',
    'custom_url',
  ]);
  assert.deepEqual(channelSchema.credentialEgressFailPolicySchema.options, [
    'fail_closed',
    'backup_proxy',
    'pool_fallback',
  ]);
  assert.deepEqual(channelSchema.channelKeySelectionStrategySchema.options, [
    'trace_sticky',
    'cache_affinity',
    'round_robin',
    'random',
    'quota_reset_priority',
    'smart',
  ]);
  assert.deepEqual(channelSchema.channelBalanceProbeScriptModeSchema.options, [
    'none',
    'expr',
    'command',
  ]);

  const keysDialogSource = readFileSync(join(srcRoot, 'features/channels/components/channels-keys-dialog.tsx'), 'utf8');
  for (const preset of [
    'deepseek_balance',
    'siliconflow_user_info',
    'moonshot_balance',
    'openrouter_credits',
    'nanogpt_check_balance',
    'tavily_usage',
    'kimi_code_usages',
    'exa_team_credits',
    'firecrawl_credits',
    'custom',
  ]) {
    assert.match(keysDialogSource, new RegExp(`['"]${preset}['"]`));
  }
});

test('API key schemas default and accept exposeUpstreamKeyQuota while preserving clearAllowedIps', () => {
  const parsedKey = apiKeySchema.apiKeySchema.parse({
    id: 'gid://axonhub/APIKey/1',
    createdAt: '2026-11-14T00:00:00Z',
    updatedAt: '2026-11-14T00:00:00Z',
    key: 'sk-axon-test',
    name: 'Client Key',
    type: 'user',
    status: 'enabled',
  });
  assert.equal(parsedKey.exposeUpstreamKeyQuota, true);

  const parsedCreate = apiKeySchema.createApiKeyInputSchema.parse({
    name: 'New Key',
    exposeUpstreamKeyQuota: false,
  });
  assert.equal(parsedCreate.exposeUpstreamKeyQuota, false);

  const parsedUpdate = apiKeySchema.updateApiKeyInputSchema.parse({
    name: 'Edited Key',
    clearAllowedIps: true,
    exposeUpstreamKeyQuota: false,
  });
  assert.equal(parsedUpdate.exposeUpstreamKeyQuota, false);
  assert.equal(parsedUpdate.clearAllowedIps, true);
});

test('GraphQL queries and mutations project credentialPolicies, balanceProbe.script, outboundProxies, and exposeUpstreamKeyQuota', () => {
  const channelsSource = readFileSync(join(srcRoot, 'features/channels/data/channels.ts'), 'utf8');
  assert.match(channelsSource, /credentialPolicies\s*\{\s*\$\{CHANNEL_CREDENTIAL_POLICY_FIELDS\}\s*\}/);
  assert.match(channelsSource, /script\s*\{\s*mode\s+expression\s+command\s*\}/);
  const sharedSettingsFragmentUses = channelsSource.match(/\$\{CHANNEL_KEY_HEALTH_CHECK_FIELDS\}/g) ?? [];
  assert.ok(
    sharedSettingsFragmentUses.length >= 7,
    `expected CHANNEL_KEY_HEALTH_CHECK_FIELDS in at least 7 channel queries/mutations, got ${sharedSettingsFragmentUses.length}`
  );
  assert.match(channelsSource, /mutation\s+UpsertChannelCredentialPolicy/);

  const apiKeysSource = readFileSync(join(srcRoot, 'features/apikeys/data/apikeys.ts'), 'utf8');
  const quotaSelections = apiKeysSource.match(/\bexposeUpstreamKeyQuota\b/g) ?? [];
  assert.equal(quotaSelections.length, 5, 'expected exposeUpstreamKeyQuota in list, detail, create, update, and rotate');

  const outboundSource = readFileSync(join(srcRoot, 'features/system/data/outbound-proxies.ts'), 'utf8');
  for (const field of [
    'outboundProxies',
    'outboundProxyPools',
    'createOutboundProxy',
    'updateOutboundProxy',
    'deleteOutboundProxy',
    'probeOutboundProxy',
    'credentialReferenceCount',
    'primaryCredentialCount',
    'backupCredentialCount',
    'backupProxyId',
    'maxCredentials',
  ]) {
    assert.match(outboundSource, new RegExp(`\\b${field}\\b`));
  }
});

test('all control-plane i18n keys exist in en and zh-CN bundles with zero emojis', () => {
  const en = loadLocaleBundle('en');
  const zh = loadLocaleBundle('zh-CN');

  const targetFiles = [
    'features/system/data/outbound-proxies.ts',
    'features/system/components/outbound-proxies-settings.tsx',
    'features/system/components/outbound-proxy-dialog.tsx',
    'features/channels/components/channel-credential-policy-sheet.tsx',
    'features/channels/components/channels-key-routing-panel.tsx',
    'features/channels/components/channels-system-settings-dialog.tsx',
    'features/apikeys/components/apikeys-columns.tsx',
    'features/apikeys/components/apikeys-create-dialog.tsx',
    'features/apikeys/components/apikeys-edit-dialog.tsx',
  ];

  const staticKeys = new Set();
  for (const rel of targetFiles) {
    const content = readFileSync(join(srcRoot, rel), 'utf8');
    for (const match of content.matchAll(/(?:\bt|i18n\.t)\(\s*['"]([a-zA-Z0-9._-]+)['"]/g)) {
      staticKeys.add(match[1]);
    }
  }

  for (const dynamicKey of [
    'system.outboundProxies.kind.static',
    'system.outboundProxies.kind.dynamic_template',
    'channels.dialogs.keyRouting.strategies.quota_reset_priority.label',
    'channels.dialogs.keyRouting.strategies.quota_reset_priority.description',
    'channels.dialogs.keyRouting.strategies.smart.label',
    'channels.dialogs.keyRouting.strategies.smart.description',
    'channels.dialogs.keys.balanceProbe.presets.tavily_usage',
    'channels.dialogs.keys.balanceProbe.presets.kimi_code_usages',
    'channels.dialogs.keys.balanceProbe.presets.exa_team_credits',
    'channels.dialogs.keys.balanceProbe.presets.firecrawl_credits',
    'channels.dialogs.keys.balanceProbe.script.modeLabel',
    'channels.dialogs.keys.balanceProbe.script.modes.none',
    'channels.dialogs.keys.balanceProbe.script.modes.expr',
    'channels.dialogs.keys.balanceProbe.script.modes.command',
    'channels.dialogs.keys.balanceProbe.script.expressionLabel',
    'channels.dialogs.keys.balanceProbe.script.expressionDescription',
    'channels.dialogs.keys.balanceProbe.script.commandLabel',
    'channels.dialogs.keys.balanceProbe.script.commandDescription',
    'channels.dialogs.keys.actions.configurePolicy',
    'channels.dialogs.keys.policy.saveSuccess',
    'channels.dialogs.keys.policy.egressModes.inherit_channel',
    'channels.dialogs.keys.policy.egressModes.direct',
    'channels.dialogs.keys.policy.egressModes.pinned_proxy',
    'channels.dialogs.keys.policy.egressModes.proxy_pool',
    'channels.dialogs.keys.policy.egressModes.custom_url',
    'channels.dialogs.keys.policy.failPolicies.fail_closed',
    'channels.dialogs.keys.policy.failPolicies.backup_proxy',
    'channels.dialogs.keys.policy.failPolicies.pool_fallback',
    'channels.dialogs.keys.policy.failPolicies.fail_closedHint',
    'channels.dialogs.keys.policy.failPolicies.backup_proxyHint',
    'channels.dialogs.keys.policy.failPolicies.pool_fallbackHint',
  ]) {
    staticKeys.add(dynamicKey);
  }

  const emojiPattern = /\p{Extended_Pictographic}/u;

  for (const key of staticKeys) {
    assert.ok(key in en, `missing English translation for ${key}`);
    assert.ok(key in zh, `missing Chinese translation for ${key}`);
    if (
      key.startsWith('system.outboundProxies.') ||
      key.startsWith('channels.dialogs.keys.policy.') ||
      key.startsWith('channels.dialogs.keys.balanceProbe.script.') ||
      key.includes('exposeUpstreamKeyQuota') ||
      key.startsWith('apikeys.quotaExposure.') ||
      key === 'apikeys.columns.quotaExposure'
    ) {
      assert.equal(emojiPattern.test(en[key]), false, `unexpected emoji in en[${key}]: ${en[key]}`);
      assert.equal(emojiPattern.test(zh[key]), false, `unexpected emoji in zh[${key}]: ${zh[key]}`);
    }
  }
});
