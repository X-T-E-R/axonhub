import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import test from 'node:test';
import ts from 'typescript';

const srcRoot = join(import.meta.dirname, '..', '..');
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
      resolvedHref = await resolveModuleUrl(`${specifier.slice(2)}.ts`);
    } else if (specifier.startsWith('./') || specifier.startsWith('../')) {
      const baseDir = join(relativePath, '..');
      const joined = join(baseDir, `${specifier}.ts`).replaceAll('\\', '/');
      resolvedHref = await resolveModuleUrl(joined);
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
  return import(await resolveModuleUrl(relativePath));
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
const quotaFormat = await importTypeScript('lib/quota-format.ts');

test('hasRealWindowMeter and hasRealWalletMeter never fabricate meters without real denominators', () => {
  assert.equal(quotaFormat.hasRealWindowMeter({ key: '5h', label: '5h', used: 10, limit: 100 }), true);
  assert.equal(quotaFormat.hasRealWindowMeter({ key: '7d', label: '7d', usageRatio: 0.42 }), true);
  assert.equal(quotaFormat.hasRealWindowMeter({ key: 'w', label: 'Wallet', remaining: 50 }), false);
  assert.equal(quotaFormat.hasRealWindowMeter({ key: 'w', label: 'Wallet', limit: 0, used: 0 }), false);

  assert.equal(
    quotaFormat.hasRealWalletMeter({ unlimited: true, walletLimit: 100, walletRemaining: 100 }),
    false
  );
  assert.equal(
    quotaFormat.hasRealWalletMeter({ freeTier: true, walletLimit: 100, walletUsed: 0.37 }),
    false
  );
  assert.equal(
    quotaFormat.hasRealWalletMeter({ unlimited: false, freeTier: false, walletLimit: 1000, walletUsed: 1001, walletRemaining: 0 }),
    true
  );
  assert.equal(
    quotaFormat.hasRealWalletMeter({ unlimited: false, freeTier: false, walletLimit: null, walletRemaining: 44.2 }),
    false
  );
});

test('mergeChannelSettingsForUpdate preserves sessionAffinity, idRewriteRules, and write-only probeAPIKey safely', () => {
  const existing = {
    sessionAffinity: {
      enabled: true,
      ttlSeconds: 3600,
      anchors: ['header:X-Msh-Session-Id', 'json:metadata.user_id'],
      continuationGuard: 'strict_replay',
    },
    idRewriteRules: [
      { target: 'header:X-Msh-Device-Id', op: 'derive', when: 'always', format: 'hex32', ifMissing: 'skip' },
    ],
    balanceProbe: {
      enabled: false,
      preset: 'exa_team_credits',
      sharedWallet: true,
      hasProbeAPIKey: true,
    },
    keyHealthCheck: {
      enabled: true,
      sharedBalanceSnapshot: { checkedAt: '2026-11-24T00:00:00Z', success: true },
      sharedProbeStatus: 'ok',
    },
  };

  // Unrelated edit must preserve sessionAffinity, idRewriteRules, sharedWallet and strip read-only hasProbeAPIKey
  const unrelated = merge.mergeChannelSettingsForUpdate(existing, { disableRetries: true });
  assert.deepEqual(unrelated.sessionAffinity, existing.sessionAffinity);
  assert.deepEqual(unrelated.idRewriteRules, existing.idRewriteRules);
  assert.equal(unrelated.balanceProbe.sharedWallet, true);
  assert.equal('hasProbeAPIKey' in unrelated.balanceProbe, false);
  assert.equal('probeAPIKeyOp' in unrelated.balanceProbe, false);
  assert.equal('sharedBalanceSnapshot' in unrelated.keyHealthCheck, false);

  // Explicit clear & set probeAPIKeyOp forward cleanly
  const clearedSecret = merge.mergeChannelSettingsForUpdate(existing, {
    balanceProbe: { enabled: false, preset: 'exa_team_credits', sharedWallet: true, probeAPIKeyOp: 'clear' },
  });
  assert.equal(clearedSecret.balanceProbe.probeAPIKeyOp, 'clear');

  const setSecret = merge.mergeChannelSettingsForUpdate(existing, {
    balanceProbe: {
      enabled: true,
      preset: 'exa_team_credits',
      sharedWallet: true,
      probeAPIKeyOp: 'set',
      probeAPIKey: '  exa-admin-secret  ',
    },
  });
  assert.equal(setSecret.balanceProbe.probeAPIKeyOp, 'set');
  assert.equal(setSecret.balanceProbe.probeAPIKey, 'exa-admin-secret');

  // Explicit empty idRewriteRules and disabled sessionAffinity are preserved as explicit clears
  const clearedRules = merge.mergeChannelSettingsForUpdate(existing, {
    sessionAffinity: { enabled: false, ttlSeconds: 3600, anchors: [], continuationGuard: 'strict_replay' },
    idRewriteRules: [],
  });
  assert.equal(clearedRules.sessionAffinity.enabled, false);
  assert.deepEqual(clearedRules.idRewriteRules, []);
});

test('list queries drop keyMetadata/history, select quotaOverview, and safe quota inventory omits rawKey/rawSummary/history', () => {
  const channelsSource = readFileSync(join(srcRoot, 'features/channels/data/channels.ts'), 'utf8');
  const healthFragMatch = channelsSource.match(/const CHANNEL_KEY_HEALTH_CHECK_FIELDS = `([\s\S]*?)`;/);
  assert.ok(healthFragMatch, 'CHANNEL_KEY_HEALTH_CHECK_FIELDS must exist');
  const healthFrag = healthFragMatch[1];
  assert.doesNotMatch(healthFrag, /\bkeyMetadata\b/, 'list fragment must not fetch keyMetadata');
  assert.doesNotMatch(healthFrag, /\barchivedKeys\b/, 'list fragment must not fetch archivedKeys');
  assert.doesNotMatch(healthFrag, /\bhistory\s*\{/, 'list fragment must not fetch nested history');
  assert.match(healthFrag, /\bsessionAffinity\s*\{/);
  assert.match(healthFrag, /\bidRewriteRules\s*\{/);
  assert.match(healthFrag, /\bsharedWallet\b/);
  assert.match(healthFrag, /\bhasProbeAPIKey\b/);

  const quotaInvMatch = channelsSource.match(/const CHANNEL_QUOTA_KEY_INVENTORY_FIELDS = `([\s\S]*?)`;/);
  assert.ok(quotaInvMatch, 'CHANNEL_QUOTA_KEY_INVENTORY_FIELDS must exist');
  assert.doesNotMatch(quotaInvMatch[1], /\brawKey\b/);
  assert.doesNotMatch(quotaInvMatch[1], /\brawSummary\b/);
  assert.doesNotMatch(quotaInvMatch[1], /\bhistory\s*\{/);

  const quotasDataSource = readFileSync(join(srcRoot, 'features/quotas/data/quotas.ts'), 'utf8');
  assert.doesNotMatch(quotasDataSource, /\bkeyMetadata\b/);
  assert.doesNotMatch(quotasDataSource, /\bchannelAPIKeyInventory\b/);
  assert.match(quotasDataSource, /\bquotaOverview\s*\{/);
});

test('/quotas route is protected by read_channels in sidebar, route-permission, and RouteGuard', () => {
  const sidebar = readFileSync(join(srcRoot, 'sidebar.ts'), 'utf8');
  const routePerm = readFileSync(join(srcRoot, 'config/route-permission.ts'), 'utf8');
  const quotasRoute = readFileSync(join(srcRoot, 'routes/_authenticated/quotas/index.tsx'), 'utf8');

  assert.match(sidebar, /url:\s*['"]\/quotas['"]/);
  assert.match(routePerm, /path:\s*['"]\/quotas['"][\s\S]*?requiredScopes:\s*\['read_channels'\]/);
  assert.match(quotasRoute, /<RouteGuard\s+requiredScopes=\{\['read_channels'\]\}\s+scopeLevel=['"]system['"]/);
});

test('all new quota and routing i18n keys exist in both en and zh-CN with zero UI emojis', () => {
  const en = loadLocaleBundle('en');
  const zh = loadLocaleBundle('zh-CN');
  const emojiPattern = /\p{Extended_Pictographic}/u;

  const targetFiles = [
    'components/quota-window-bars.tsx',
    'components/quota-badges.tsx',
    'features/quotas/index.tsx',
    'features/quotas/components/quota-channel-keys-panel.tsx',
    'features/channels/components/channel-quota-cell.tsx',
    'features/channels/components/channels-key-routing-panel.tsx',
  ];

  const keys = new Set([
    'sidebar.items.quotas',
    'channels.columns.quota',
    'channels.dialogs.keys.summary.activeBalanceShared',
    'channels.dialogs.keys.balanceProbe.presets.command_code_billing',
    'channels.dialogs.keys.balanceProbe.presets.step_plan_usage',
    'channels.dialogs.keys.balanceProbe.presets.newapi_token_usage',
    'channels.dialogs.keys.balanceProbe.presets.openai_billing_usage',
    'channels.dialogs.keys.balanceProbe.presets.sub2_usage',
    'channels.dialogs.keys.balanceProbe.sharedWallet.label',
    'channels.dialogs.keys.balanceProbe.sharedWallet.description',
    'channels.dialogs.keys.balanceProbe.probeAPIKey.label',
    'channels.dialogs.keys.balanceProbe.probeAPIKey.configured',
    'channels.dialogs.keys.balanceProbe.probeAPIKey.notConfigured',
    'channels.dialogs.keys.balanceProbe.probeAPIKey.replace',
    'channels.dialogs.keys.balanceProbe.probeAPIKey.clear',
    'channels.dialogs.keys.balanceProbe.exaFixedEndpointNotice',
    'quotas.state.available',
    'quotas.state.warning',
    'quotas.state.exhausted',
    'quotas.state.free_tier',
    'quotas.state.unlimited',
    'quotas.state.unknown',
    'quotas.state.error',
    'quotas.state.unsupported',
    'quotas.source.manual',
    'quotas.source.balance_probe',
    'quotas.source.provider_quota',
    'quotas.source.none',
  ]);

  for (const rel of targetFiles) {
    const content = readFileSync(join(srcRoot, rel), 'utf8');
    for (const match of content.matchAll(/\bt\(\s*['"]([a-zA-Z0-9._-]+)['"]/g)) {
      keys.add(match[1]);
    }
    for (const match of content.matchAll(/['"](channels\.dialogs\.keyRouting\.validation\.[a-zA-Z0-9._-]+)['"]/g)) {
      keys.add(match[1]);
    }
  }

  for (const key of keys) {
    assert.ok(key in en, `missing en translation for ${key}`);
    assert.ok(key in zh, `missing zh-CN translation for ${key}`);
    assert.equal(emojiPattern.test(en[key]), false, `unexpected emoji in en[${key}]: ${en[key]}`);
    assert.equal(emojiPattern.test(zh[key]), false, `unexpected emoji in zh[${key}]: ${zh[key]}`);
  }
});

test('getQuotaKeyAvailability subtracts exhausted keys from active schedulable count and exposes disabled count', () => {
  const src = readFileSync(join(srcRoot, 'lib/quota-format.ts'), 'utf8');
  assert.match(src, /schedulable:\s*Math\.max\(0,\s*active\s*-\s*exhausted\)/);
  assert.match(src, /disabled:\s*Math\.max\(0,\s*total\s*-\s*active\)/);

  for (const rel of [
    'features/quotas/index.tsx',
    'features/channels/components/channel-quota-cell.tsx',
    'components/quota-badges.tsx',
  ]) {
    const content = readFileSync(join(srcRoot, rel), 'utf8');
    assert.match(content, /getQuotaKeyAvailability\(/, `${rel} must use unified getQuotaKeyAvailability helper`);
    assert.doesNotMatch(content, /counts\.active\}\/\{[^}]*counts\.total/, `${rel} must not display raw active/total without subtracting exhausted`);
  }
});

test('channelBalanceProbeSchema and routing schemas accept Go GraphQL zero-values ("", 0)', () => {
  const schemaSrc = readFileSync(join(srcRoot, 'features/channels/data/schema.ts'), 'utf8');
  assert.match(schemaSrc, /const emptyStringToNull =/);
  assert.match(schemaSrc, /const zeroOrEmptyToNull =/);
  assert.match(schemaSrc, /primarySelection:\s*z\.preprocess\(emptyStringToNull/);
  assert.match(schemaSrc, /timeoutMs:\s*z\.preprocess\(zeroOrEmptyToNull/);
});

test('expanded key exhaustion respects window resetAt expiry, shared wallet deduplicates canonically, and routing-only save preserves untouched balanceProbe', () => {
  const fmtSrc = readFileSync(join(srcRoot, 'lib/quota-format.ts'), 'utf8');
  assert.match(fmtSrc, /export function isResetExpired\(/);
  assert.match(fmtSrc, /export function isQuotaKeyExhausted\(/);

  const panelSrc = readFileSync(join(srcRoot, 'features/quotas/components/quota-channel-keys-panel.tsx'), 'utf8');
  assert.match(panelSrc, /isQuotaKeyExhausted\(item,\s*nowMs\)/);

  const keysDialogSrc = readFileSync(join(srcRoot, 'features/channels/components/channels-keys-dialog.tsx'), 'utf8');
  assert.match(keysDialogSrc, /const walletKey = isShared \? '__shared_wallet__'/);
  assert.match(keysDialogSrc, /const balanceProbeChanged =/);

  const badgesSrc = readFileSync(join(srcRoot, 'components/quota-badges.tsx'), 'utf8');
  assert.match(badgesSrc, /readSharedQuotaBadgeChannels\(/);
  assert.match(badgesSrc, /quotas\.counts\.exhausted/);
});
