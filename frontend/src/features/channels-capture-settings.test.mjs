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

test('combined AxonHub behavior presets keep passthrough, retries, and capture coherent', () => {
  assert.deepEqual(capture.AXONHUB_BEHAVIOR_PRESETS.lowLatency, {
    passThroughUserAgent: false,
    passThroughBody: true,
    disableRetries: true,
    fullPassThrough: true,
    storeExecutionRequestBody: false,
    storeExecutionResponseBody: false,
    storeExecutionStreamChunks: false,
  });
  assert.deepEqual(capture.AXONHUB_BEHAVIOR_PRESETS.audit, {
    passThroughUserAgent: null,
    passThroughBody: false,
    disableRetries: false,
    fullPassThrough: true,
    storeExecutionRequestBody: true,
    storeExecutionResponseBody: true,
    storeExecutionStreamChunks: false,
  });
});

test('combined AxonHub behavior derives presets and detects custom capture combinations', () => {
  const standard = capture.resolveAxonHubBehaviorPreset('standard');
  assert.deepEqual(standard, capture.AXONHUB_BEHAVIOR_PRESETS.standard);
  assert.equal(capture.getAxonHubBehaviorPreset(standard), 'standard');
  assert.equal(capture.resolveAxonHubBehaviorPreset('custom'), null);
  assert.equal(
    capture.getAxonHubBehaviorPreset({ ...standard, storeExecutionResponseBody: false }),
    'custom'
  );
});

for (const targetProvider of ['OpenAI', 'Codex']) {
  test(`leaving AxonHub for ${targetProvider} restores generic settings instead of leaking the low-latency preset`, () => {
    const genericSettings = {
      passThroughUserAgent: true,
      passThroughBody: false,
      disableRetries: false,
    };
    const entered = capture.transitionAxonHubScopedSettings(genericSettings, null, false, true);
    assert.equal(entered.enteredAxonHub, true);
    assert.deepEqual(entered.restoreSettings, genericSettings);

    const lowLatency = capture.AXONHUB_BEHAVIOR_PRESETS.lowLatency;
    const left = capture.transitionAxonHubScopedSettings(lowLatency, entered.restoreSettings, true, false);
    assert.equal(left.leftAxonHub, true);
    assert.deepEqual(left.settings, genericSettings);
    assert.deepEqual(capture.resolveGenericChannelBehaviorSettings(false, lowLatency, entered.restoreSettings), genericSettings);
  });
}

test('an edited AxonHub channel restores its persisted generic baseline after applying a preset', () => {
  const persisted = capture.getGenericChannelBehaviorSettings({
    passThroughUserAgent: true,
    passThroughBody: false,
    disableRetries: false,
  });
  const left = capture.transitionAxonHubScopedSettings(capture.AXONHUB_BEHAVIOR_PRESETS.lowLatency, persisted, true, false);
  assert.deepEqual(left.settings, persisted);
});

test('a transition without a prior generic snapshot uses neutral defaults', () => {
  const left = capture.transitionAxonHubScopedSettings(capture.AXONHUB_BEHAVIOR_PRESETS.lowLatency, null, true, false);
  assert.deepEqual(left.settings, capture.DEFAULT_GENERIC_CHANNEL_BEHAVIOR_SETTINGS);
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

test('unrelated channel updates preserve proxy reuse while stripping key-health runtime state', () => {
  const existing = {
    proxy: {
      type: 'url',
      url: 'http://proxy.example',
      disableConnectionReuse: true,
    },
    keyHealthCheck: {
      enabled: true,
      historyLimit: 20,
      keyMetadata: [{ keyId: 'key_1', status: 'healthy' }],
      archivedKeys: [{ keyId: 'key_2' }],
      history: [{ keyId: 'key_1', success: true }],
    },
  };

  const updated = merge.mergeChannelSettingsForUpdate(existing, { disableRetries: true });

  assert.deepEqual(updated.proxy, existing.proxy);
  assert.deepEqual(updated.keyHealthCheck, {
    enabled: true,
    historyLimit: 20,
  });
});
