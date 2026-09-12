import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

test('Codex agent tool aliases are read, edited, and written by the transform options dialog', () => {
  const schema = read('features/channels/data/schema.ts');
  const dialog = read('features/channels/components/channels-transform-options-dialog.tsx');

  assert.match(schema, /codexAgentToolAliases:\s*z\.boolean\(\)\.optional\(\)/);
  assert.match(dialog, /name='codexAgentToolAliases'/);
  assert.match(
    dialog,
    /codexAgentToolAliases:\s*currentRow\.settings\?\.transformOptions\?\.codexAgentToolAliases \|\| false/g
  );
  assert.match(dialog, /codexAgentToolAliases:\s*values\.codexAgentToolAliases/);
});

test('every channel GraphQL result that echoes transform options selects the alias switch', () => {
  const operations = read('features/channels/data/channels.ts');
  const selections = [...operations.matchAll(/transformOptions\s*\{([^}]+)\}/g)].map((match) => match[1]);

  assert.equal(selections.length, 7);
  for (const selection of selections) {
    assert.match(selection, /\bcodexAgentToolAliases\b/);
  }
});

test('Codex agent tool alias labels exist in both channel locale bundles', () => {
  for (const locale of ['en', 'zh-CN']) {
    const messages = JSON.parse(read(`locales/${locale}/channels.json`));
    assert.equal(typeof messages['channels.dialogs.fields.transformOptions.codexAgentToolAliases.label'], 'string');
    assert.equal(typeof messages['channels.dialogs.fields.transformOptions.codexAgentToolAliases.description'], 'string');
  }
});
