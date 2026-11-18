import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..', '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

function parseLocale(locale) {
  return JSON.parse(read(`locales/${locale}/channels.json`));
}

test('credential source data contract is echoed and sent through channel operations', () => {
  const schema = read('features/channels/data/schema.ts');
  const channelsData = read('features/channels/data/channels.ts');
  const merge = read('features/channels/utils/merge.ts');

  assert.match(schema, /credentialSourceChannelId:\s*z\.number\(\)\.int\(\)\.optional\(\)\.nullable\(\)/);
  assert.match(schema, /!hasCredentialSource && !hasApiKey && !hasApiKeys/);
  assert.match(channelsData, /credentialSourceChannelId/);
  assert.ok((channelsData.match(/credentialSourceChannelId/g) ?? []).length >= 8);
  assert.match(merge, /credentialSourceChannelId:\s*pick\('credentialSourceChannelId'/);
});

test('candidate source selection excludes self, archived channels, and delegating channels while retaining disabled owners', () => {
  const actionDialog = read('features/channels/components/channels-action-dialog.tsx');

  assert.match(actionDialog, /useAllChannelSummarys\(undefined, \{ enabled: open, includeArchived: false \}\)/);
  assert.match(actionDialog, /extractNumberID\(channel\.id\) !== extractNumberID\(initialRow\?\.id/);
  assert.match(actionDialog, /channel\.status !== 'archived'/);
  assert.match(actionDialog, /channel\.settings\?\.credentialSourceChannelId == null/);
  assert.match(actionDialog, /channel\.status === 'disabled'/);
});

test('dependent key management keeps inventory/checks and offers explicit detachment', () => {
  const keysDialog = read('features/channels/components/channels-keys-dialog.tsx');

  assert.match(keysDialog, /channels\.dialogs\.keys\.sourceNotice/);
  assert.match(keysDialog, /channels\.dialogs\.keys\.detachSource/);
  assert.match(keysDialog, /credentialSourceChannelId: null/);
  assert.match(keysDialog, /!isCredentialSourceDependent/);
  assert.match(keysDialog, /handleRunChecks/);
});

test('credential source UI copy exists in English and Chinese', () => {
  for (const locale of ['en', 'zh-CN']) {
    const messages = parseLocale(locale);
    assert.ok(messages['channels.dialogs.fields.credentialSource.label']);
    assert.ok(messages['channels.dialogs.fields.credentialSource.own']);
    assert.ok(messages['channels.dialogs.keys.sourceNotice']);
    assert.ok(messages['channels.dialogs.keys.detachSource']);
  }
});
