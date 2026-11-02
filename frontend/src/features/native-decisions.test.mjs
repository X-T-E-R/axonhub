import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, join, resolve } from 'node:path';
import test from 'node:test';
import ts from 'typescript';

const srcRoot = resolve(import.meta.dirname, '..');
const nativeRequire = createRequire(import.meta.url);
const cache = new Map();
const read = (file) => readFileSync(join(srcRoot, file), 'utf8');
// Execute the production data/schema/helpers. Icons are presentation-only stubs.
function load(file) {
  const path = resolve(srcRoot, file);
  if (cache.has(path)) return cache.get(path);
  const source = readFileSync(path, 'utf8');
  const output = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023 } });
  const module = { exports: {} };
  cache.set(path, module.exports);
  new Function('require', 'module', 'exports', output.outputText)(
    (name) => {
      if (name === '@lobehub/icons' || name.includes('-icon')) return new Proxy({}, { get: () => () => null });
      if (name.startsWith('@/') || name.startsWith('.')) {
        const target = name.startsWith('@/') ? join(srcRoot, name.slice(2)) : resolve(dirname(path), name);
        return load(target.endsWith('.ts') ? target : target + '.ts');
      }
      return nativeRequire(name);
    },
    module,
    module.exports
  );
  return module.exports;
}
const native = load('features/channels/data/native-decisions.ts');
const schema = load('features/channels/data/schema.ts');
const providers = load('features/channels/data/config_providers.ts');
const curl = load('features/requests/utils/curl-generator.ts');
const { nativeDecisionUsage } = load('features/requests/utils/native-decision-usage.ts');

test('native presets create independent saved channel endpoint payloads and reopen them', () => {
  for (const [key, preset] of Object.entries(native.NATIVE_DECISION_PRESETS)) {
    assert.equal(providers.getChannelTypeForApiFormat(key, preset.apiFormat), 'openai');
    assert.deepEqual(providers.getApiFormatsForProvider(key), [preset.apiFormat]);
    const created = schema.createChannelInputSchema.parse({
      name: preset.name,
      type: 'openai',
      baseURL: preset.baseURL || 'http://localhost:8080',
      credentials: { apiKeys: ['synthetic-' + key] },
      supportedModels: preset.models.length ? [...preset.models] : ['agr-flash'],
      defaultTestModel: preset.models[0] || 'agr-flash',
      endpoints: native.nativeDecisionEndpoints(key),
    });
    const echo = JSON.parse(JSON.stringify(created));
    assert.equal(native.getNativeDecisionPreset(echo), key);
    const updated = schema.updateChannelInputSchema.parse({
      ...echo,
      name: echo.name + ' edited',
      endpoints: native.nativeDecisionEndpoints(key, echo.endpoints),
    });
    assert.deepEqual(updated.endpoints, echo.endpoints);
    assert.equal(updated.credentials.apiKeys[0], 'synthetic-' + key);
  }
  assert.equal(
    native.NATIVE_DECISION_PRESETS.commandcode.baseURL + native.NATIVE_DECISION_PRESETS.commandcode.path,
    'https://api.commandcode.ai/provider/v1/systemone'
  );
  assert.equal(native.NATIVE_DECISION_PRESETS.typesafe.baseURL, 'https://api.typesafe.ai');
  assert.equal(native.NATIVE_DECISION_PRESETS.openai_decisions.path, '/decisions');
  assert.equal(native.isNativeDecisionPreset('toString'), false);
});

test('native reopen preserves custom endpoint path/base URL and mixed native surfaces', () => {
  const endpoints = [
    { apiFormat: 'typesafe/systemone', path: '/custom', baseURL: 'https://synthetic.invalid/root' },
    { apiFormat: 'openai/decisions', path: '/decisions' },
  ];
  assert.deepEqual(native.nativeDecisionEndpoints('systemone_compatible', endpoints), endpoints);
  assert.equal(native.getNativeDecisionPreset({ type: 'openai', baseURL: 'https://synthetic.invalid', endpoints }), 'systemone_compatible');
  assert.equal(
    native.getNativeDecisionPreset({ type: 'openai', endpoints: [...endpoints, { apiFormat: 'openai/chat_completions' }] }),
    undefined
  );
  assert.equal(native.getNativeDecisionPreset({ type: 'openai', endpoints: [] }), undefined);
  assert.deepEqual(native.nativeDecisionEndpoints('commandcode', endpoints), [
    { apiFormat: 'commandcode/systemone', path: '/v1/systemone' },
  ]);
});

test('create/edit/duplicate use shared endpoint payload and query endpoint echoes', () => {
  const dialog = read('features/channels/components/channels-action-dialog.tsx');
  const operations = read('features/channels/data/channels.ts');
  assert.match(dialog, /endpoints: nativeDecisionEndpoints\(selectedProvider, nativeEndpoints\)/);
  assert.match(dialog, /setNativeEndpoints\(initialRow.endpoints\)/);
  assert.match(dialog, /getNativeDecisionPreset\(initialRow\)/);
  assert.match(dialog, /getNativeDecisionPreset\(initialRow\)\s*\? \{ endpoints: \[\] \}/);
  assert.match(dialog, /if \(isNativeDecisionPreset\(selectedProvider\)\) return false/);
  // Behavioral provider/submit coverage lives in TestNativeDecisionAdminUI:
  // production RHF state, callbacks, mutations and persisted GraphQL reread.
  assert.match(dialog, /credentialsResetRef\.current = true/);
  assert.match(dialog, /form\.setValue\('credentials', \{ apiKey: undefined, apiKeys: \[\] \}/);
  assert.match(dialog, /getNativeDecisionPreset\(initialRow\) !== selectedProvider/);
  assert.match(dialog, /channels.nativeDecisions.providerKeyRequired/);
  const endpointSelections = operations.match(/\bendpoints\s*\{[^}]*\}/g) || [];
  assert.ok(endpointSelections.length >= 5);
  for (const selection of endpointSelections) {
    assert.match(selection, /apiFormat/);
    assert.match(selection, /path/);
    assert.match(selection, /baseURL/);
  }
});

test('unknown, partial and true-zero native usage remain different', () => {
  assert.equal(nativeDecisionUsage('openai/chat_completions', {}), undefined);
  assert.equal(nativeDecisionUsage('typesafe/systemone', {}).complete, false);
  assert.equal(nativeDecisionUsage('typesafe/systemone', { usage: { input_tokens: 12 } }).complete, false);
  assert.deepEqual(nativeDecisionUsage('typesafe/systemone', { usage: { input_tokens: 0, output_tokens: 0 } }), {
    complete: true,
    cachedTokens: null,
  });
  assert.deepEqual(
    nativeDecisionUsage('openai/decisions', { usage: { input_tokens: 4, output_tokens: 0, input_tokens_details: { cached_tokens: 0 } } }),
    { complete: true, cachedTokens: 0 }
  );
  const detail = read('features/requests/components/request-detail-content.tsx');
  assert.match(detail, /setResponseView\(decisionUsage \? 'json' : 'preview'\)/);
  assert.match(detail, /decisionUsage && !decisionUsage.complete/);
  assert.match(detail, /!decisionUsage &&[\s(]*<TabsTrigger value='preview'>/);
});

test('native cURL targets real paths and preserves original JSON strings', () => {
  const body = '{"model":"public","future":9007199254740993,"questions":{}}';
  for (const format of ['typesafe/systemone', 'commandcode/systemone', 'openai/decisions']) {
    const command = curl.generateCurlCommand({ apiFormat: format, body, baseUrl: 'https://axon.invalid' });
    assert.ok(command.includes(format === 'openai/decisions' ? '/v1/decisions' : '/v1/systemone'));
    assert.ok(command.includes(body));
    assert.ok(!command.includes('/chat/completions'));
  }
  assert.match(
    curl.generateExecutionCurl(
      {},
      body,
      { type: 'openai', baseURL: 'https://api.commandcode.ai/provider' },
      'commandcode/systemone',
      'https://api.commandcode.ai/provider/v1/systemone'
    ),
    /provider\/v1\/systemone/
  );
});

test('native capability and unknown-usage strings exist in both locale bundles', () => {
  for (const locale of ['en', 'zh-CN']) {
    const channels = JSON.parse(read(`locales/${locale}/channels.json`));
    const requests = JSON.parse(read(`locales/${locale}/requests.json`));
    for (const key of Object.keys(native.NATIVE_DECISION_PRESETS)) {
      assert.equal(typeof channels[`channels.providers.${key}`], 'string');
      assert.equal(typeof channels[`channels.nativeDecisions.${key}.description`], 'string');
    }
    assert.equal(typeof requests['requests.nativeDecisions.usageUnknown'], 'string');
    assert.equal(typeof requests['requests.nativeDecisions.notReported'], 'string');
  }
});

test('native evidence never falls back to rounded JSON; legacy scalar consumers are unchanged', () => {
  const { bodyEvidence, formatBodyEvidence } = load('features/requests/utils/body-evidence.ts');
  const rounded = { n: 9007199254740992, choice: true, name: null };
  const raw = '{"n":9007199254740993,"choice":true,"name":null}';
  for (const format of ['typesafe/systemone', 'commandcode/systemone', 'openai/decisions']) {
    const record = { format, requestBody: rounded, requestBodyRaw: raw };
    assert.equal(bodyEvidence(record, 'requestBody'), raw);
    assert.equal(formatBodyEvidence(record, 'requestBody'), raw);
    assert.equal(bodyEvidence({ format, requestBody: rounded }, 'requestBody'), '');
  }
  const ordinary = { format: 'openai/chat_completions', requestBody: rounded, requestBodyRaw: raw };
  assert.equal(bodyEvidence(ordinary, 'requestBody'), rounded);
  assert.equal(formatBodyEvidence(ordinary, 'requestBody'), JSON.stringify(rounded, null, 2));
});
