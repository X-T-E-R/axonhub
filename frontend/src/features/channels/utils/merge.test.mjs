import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import test from 'node:test';
import { mergeChannelSettingsForUpdate } from './merge.ts';

test('mergeChannelSettingsForUpdate omits untouched balanceProbe, sessionAffinity, and idRewriteRules', () => {
  const existing = {
    extraModelPrefix: 'v1',
    balanceProbe: {
      enabled: true,
      preset: 'exa_team_credits',
      sharedWallet: true,
      hasProbeAPIKey: true,
    },
    sessionAffinity: {
      enabled: true,
      ttlSeconds: 3600,
      anchors: ['header:X-Session-Id'],
      continuationGuard: 'strict_replay',
    },
    idRewriteRules: [
      {
        target: 'json:metadata.user_id',
        op: 'derive',
        when: 'always',
        format: 'anthropic_user_id',
        ifMissing: 'skip',
      },
    ],
  };

  const merged = mergeChannelSettingsForUpdate(existing, {
    extraModelPrefix: 'v2',
  });

  assert.equal(merged.extraModelPrefix, 'v2');
  assert.equal('balanceProbe' in merged, false, 'untouched balanceProbe must be omitted from update payload');
  assert.equal('sessionAffinity' in merged, false, 'untouched sessionAffinity must be omitted from update payload');
  assert.equal('idRewriteRules' in merged, false, 'untouched idRewriteRules must be omitted from update payload');
});

test('mergeChannelSettingsForUpdate preserves explicit empty idRewriteRules and explicit disabled sessionAffinity', () => {
  const existing = {
    sessionAffinity: {
      enabled: true,
      ttlSeconds: 3600,
      anchors: ['header:X-Session-Id'],
      continuationGuard: 'strict_replay',
    },
    idRewriteRules: [
      {
        target: 'header:X-Client-ID',
        op: 'derive',
        when: 'always',
        format: 'hex32',
        ifMissing: 'skip',
      },
    ],
  };

  const merged = mergeChannelSettingsForUpdate(existing, {
    sessionAffinity: {
      enabled: false,
      ttlSeconds: 3600,
      anchors: [],
      continuationGuard: 'strict_replay',
    },
    idRewriteRules: [],
  });

  assert.equal('balanceProbe' in merged, false, 'untouched balanceProbe stays omitted');
  assert.deepEqual(merged.sessionAffinity, {
    enabled: false,
    ttlSeconds: 3600,
    anchors: [],
    continuationGuard: 'strict_replay',
  });
  assert.deepEqual(merged.idRewriteRules, [], 'explicit empty array must be sent to clear rules');
});

test('mergeChannelSettingsForUpdate sanitizes balanceProbe only when explicitly patched', () => {
  const merged = mergeChannelSettingsForUpdate(null, {
    balanceProbe: {
      enabled: true,
      preset: 'exa_team_credits',
      sharedWallet: true,
      hasProbeAPIKey: true,
      probeAPIKeyOp: 'set',
      probeAPIKey: '  exa-admin-secret  ',
    },
  });

  assert.equal('balanceProbe' in merged, true);
  assert.equal('hasProbeAPIKey' in (merged.balanceProbe ?? {}), false);
  assert.equal(merged.balanceProbe?.probeAPIKeyOp, 'set');
  assert.equal(merged.balanceProbe?.probeAPIKey, 'exa-admin-secret');
});

test('channels-keys-dialog tracks baselines for balanceProbe, sessionAffinity, and idRewriteRules', () => {
  const source = readFileSync(resolve(import.meta.dirname, '../components/channels-keys-dialog.tsx'), 'utf8');
  assert.match(source, /sessionAffinityBaselineRef/);
  assert.match(source, /idRewriteRulesBaselineRef/);
  assert.match(source, /\.\.\.\(sessionAffinityChanged \? \{ sessionAffinity: nextSessionAffinity \} : \{\}\)/);
  assert.match(source, /\.\.\.\(idRewriteRulesChanged \? \{ idRewriteRules: nextIDRewriteRules \} : \{\}\)/);
  assert.match(source, /\.\.\.\(balanceProbeChanged[\s\S]*\? \{ balanceProbe: balanceProbeFromValues/);
});
