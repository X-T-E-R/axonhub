import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

test('edit dialog submits clearAllowedIps when the allowlist is emptied (toggle off included)', () => {
  const source = read('features/apikeys/components/apikeys-edit-dialog.tsx');

  // The toggle-off path normalizes to an empty allowlist...
  assert.match(
    source,
    /const allowedIps = ipRestrictionEnabled\s*\?[\s\S]*?filter\(\(s\) => s !== ''\)\s*: \[\];/,
    'turning off IP Restriction must normalize the allowlist to an empty array'
  );

  // ...and an empty allowlist is submitted as clearAllowedIps: true so stale
  // rules cannot survive a save that only turned the toggle off.
  assert.match(
    source,
    /\(\s*allowedIps\.length > 0\s*\?\s*\{\s*allowedIps\s*\}\s*:\s*\{\s*clearAllowedIps:\s*true\s*\}\s*\)/,
    'an empty allowlist must send clearAllowedIps: true'
  );
});

test('edit dialog sends allowedIps without clearing when the allowlist is non-empty', () => {
  const source = read('features/apikeys/components/apikeys-edit-dialog.tsx');

  assert.match(source, /allowedIps\.length > 0\s*\?\s*\{\s*allowedIps\s*\}/, 'a non-empty allowlist is sent as allowedIps');
  assert.match(
    source,
    /allowedIps\.length > 0\s*\?\s*\{\s*allowedIps\s*\}\s*:\s*\{\s*clearAllowedIps:\s*true\s*\}/,
    'clearAllowedIps is only used for the empty branch, never alongside allowedIps'
  );
});

test('update schemas accept an optional clearAllowedIps flag', () => {
  const source = read('features/apikeys/data/schema.ts');

  const factoryBlock = source.match(/export const updateApiKeyInputSchemaFactory[\s\S]*?\n\}\);/);
  const defaultBlock = source.match(/export const updateApiKeyInputSchema = z\.object\(\{[\s\S]*?\n\}\);/);

  assert.ok(factoryBlock, 'factory schema definition found');
  assert.ok(defaultBlock, 'default schema definition found');
  assert.match(factoryBlock[0], /clearAllowedIps: z\.boolean\(\)\.optional\(\)/, 'factory schema exposes clearAllowedIps');
  assert.match(defaultBlock[0], /clearAllowedIps: z\.boolean\(\)\.optional\(\)/, 'default schema exposes clearAllowedIps');
});

test('create dialog never submits a clear flag', () => {
  const source = read('features/apikeys/components/apikeys-create-dialog.tsx');
  assert.doesNotMatch(source, /clearAllowedIps/, 'the clear flag belongs to the update path only');
});