import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import ts from 'typescript';

const srcRoot = join(import.meta.dirname, '..');

async function importTypeScript(relativePath) {
  const source = readFileSync(join(srcRoot, relativePath), 'utf8');
  const output = ts.transpileModule(source, {
    compilerOptions: {
      module: ts.ModuleKind.ESNext,
      target: ts.ScriptTarget.ES2023,
    },
  }).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(output).toString('base64')}`);
}

const capture = await importTypeScript('features/channels/utils/capture-settings.ts');
const merge = await importTypeScript('features/channels/utils/merge.ts');

test('AxonHub execution capture defaults to inheriting the system policy', () => {
  assert.deepEqual(capture.getAxonHubCaptureSettings(undefined), capture.AXONHUB_CAPTURE_PRESETS.inherit);
  assert.equal(capture.getAxonHubCapturePreset(capture.getAxonHubCaptureSettings(undefined)), 'inherit');
});

test('full debug and low latency presets are explicit opposites', () => {
  assert.deepEqual(capture.AXONHUB_CAPTURE_PRESETS.fullDebug, {
    storeExecutionRequestBody: true,
    storeExecutionResponseBody: true,
    storeExecutionStreamChunks: true,
  });
  assert.deepEqual(capture.AXONHUB_CAPTURE_PRESETS.lowLatency, {
    storeExecutionRequestBody: false,
    storeExecutionResponseBody: false,
    storeExecutionStreamChunks: false,
  });
});

test('mixed capture settings derive a custom preset', () => {
  assert.equal(
    capture.getAxonHubCapturePreset({
      storeExecutionRequestBody: true,
      storeExecutionResponseBody: true,
      storeExecutionStreamChunks: false,
    }),
    'custom'
  );
});

test('non-preset select values cannot clear capture settings', () => {
  assert.equal(capture.resolveAxonHubCapturePreset('disabled'), null);
  assert.equal(capture.resolveAxonHubCapturePreset('custom'), null);
  assert.deepEqual(capture.resolveAxonHubCapturePreset('fullDebug'), capture.AXONHUB_CAPTURE_PRESETS.fullDebug);
});

test('unrelated channel updates preserve execution capture overrides', () => {
  const existing = {
    storeExecutionRequestBody: true,
    storeExecutionResponseBody: false,
    storeExecutionStreamChunks: null,
  };

  const updated = merge.mergeChannelSettingsForUpdate(existing, { disableRetries: true });
  assert.equal(updated.storeExecutionRequestBody, true);
  assert.equal(updated.storeExecutionResponseBody, false);
  assert.equal(updated.storeExecutionStreamChunks, null);
});
