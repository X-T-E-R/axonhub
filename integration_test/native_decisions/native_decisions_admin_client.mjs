// Real production form closures + mutation/query hooks + graphqlRequest against
// the Go fixture's authenticated /admin/graphql and persisted Ent/blob records.
import assert from 'node:assert/strict';
import { readFileSync, existsSync, statSync } from 'node:fs';
import { createRequire } from 'node:module';
import { resolve, dirname } from 'node:path';
const root = resolve(import.meta.dirname, '../../frontend/src');
const req = createRequire(resolve(root, '../package.json'));
const ts = req('typescript');
const { createFormControl } = req('react-hook-form');
const { zodResolver } = req('@hookform/resolvers/zod');
const base = process.argv[2];
assert.match(base, /^http:\/\/127\.0\.0\.1:\d+$/);
const realFetch = globalThis.fetch;
const mutations = [];
globalThis.fetch = async (url, options) => {
  if (url === '/admin/graphql') {
    const payload = JSON.parse(options.body);
    const mutation = payload.query.trim().startsWith('mutation');
    if (mutation) mutations.push(payload);
    const response = await realFetch(base + url, options);
    if (mutation) {
      const body = await response.clone().json();
      assert.ok(!body.errors, JSON.stringify(body.errors));
      payload.savedID = Object.values(body.data)[0]?.id;
    }
    return response;
  }
  throw new Error('unexpected URL ' + url);
};
const noop = () => {};
const stubs = {
  '@tanstack/react-query': { useMutation: (config) => ({ mutateAsync: config.mutationFn }), useQuery: (config) => config, useQueryClient: () => ({ invalidateQueries: noop, getQueryData: () => undefined }) },
  'react-i18next': { useTranslation: () => ({ t: (s) => s }) },
  sonner: { toast: { error: (s) => errors.push(s), success: noop } },
  '@/hooks/use-error-handler': { useErrorHandler: () => ({ handleError: (e) => { throw e; } }) },
  '@/stores/authStore': { getTokenFromStorage: () => process.env.AXON_NATIVE_ADMIN_TOKEN, removeTokenFromStorage: noop },
  '@/lib/i18n': { __esModule: true, default: { t: (s) => s } },
  '@/stores/projectStore': { useSelectedProjectId: () => null },
  '../../../hooks/useRequestPermissions': { useRequestPermissions: () => ({ canViewApiKeys: true, canViewChannels: true }) },
};
const errors = [];
const cache = new Map();
function load(file) {
  let path = resolve(root, file);
  if (!existsSync(path) || statSync(path).isDirectory()) path += existsSync(path + '.ts') ? '.ts' : existsSync(path + '.tsx') ? '.tsx' : '/index.ts';
  if (cache.has(path)) return cache.get(path).exports;
  const module = { exports: {} }; cache.set(path, module);
  const source = readFileSync(path, 'utf8');
  const output = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  new Function('require', 'module', 'exports', output)((name) => {
    if (Object.hasOwn(stubs, name)) return stubs[name];
    if (name === '@lobehub/icons' || name.includes('-icon')) return new Proxy({}, { get: () => () => null });
    if (name.startsWith('@/') || name.startsWith('.')) return load(name.startsWith('@/') ? name.slice(2) : resolve(dirname(path), name));
    return req(name);
  }, module, module.exports);
  return module.exports;
}
function ast(file) { return ts.createSourceFile(file, readFileSync(resolve(root, file), 'utf8'), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX); }
function evaluate(text, context) {
  const code = ts.transpileModule(`module.exports = ${text};`, { compilerOptions: { target: ts.ScriptTarget.ES2023, module: ts.ModuleKind.CommonJS } }).outputText;
  const module = { exports: {} };
  new Function(...Object.keys(context), 'module', code)(...Object.values(context), module);
  return module.exports;
}
function closure(file, name, context, callback = false) {
  const tree = ast(file); let expr;
  function visit(node) { if (ts.isVariableDeclaration(node) && node.name.getText(tree) === name) expr = (callback ? node.initializer.arguments[0] : node.initializer).getText(tree); ts.forEachChild(node, visit); }
  visit(tree); assert.ok(expr, name); return evaluate(expr, context);
}
const native = load('features/channels/data/native-decisions.ts');
const providers = load('features/channels/data/config_providers.ts');
const configs = load('features/channels/data/config_channels.ts');
const schema = load('features/channels/data/schema.ts');
const capture = load('features/channels/utils/capture-settings.ts');
const merge = load('features/channels/utils/merge.ts');
const channels = load('features/channels/data/channels.ts');
const gql = load('gql/graphql.ts');
const dialog = 'features/channels/components/channels-action-dialog.tsx';
async function readChannel(id) {
  return (await gql.graphqlRequest('query ReadNativeChannel($id: ID!) { node(id:$id) { ... on Channel { id type name baseURL supportedModels defaultTestModel endpoints { apiFormat path baseURL } credentials { apiKey apiKeys } } } }', { id })).node;
}
let serial = 0;
function formCase(mode, initialRow) {
  const isEdit = mode === 'update', isDuplicate = mode === 'duplicate';
  const context = { ...native, ...providers, ...configs, ...capture, ...merge,
    form: null, initialRow, currentRow: isEdit ? initialRow : undefined, duplicateFromRow: isDuplicate ? initialRow : undefined,
    isEdit, isDuplicate, isOAuthChannel: false, selectedProvider: native.getNativeDecisionPreset(initialRow) || 'openai', selectedApiFormat: native.getNativeDecisionPreset(initialRow) ? native.NATIVE_DECISION_PRESETS[native.getNativeDecisionPreset(initialRow)].apiFormat : 'openai/chat_completions',
    responsesTransport: 'http', useGeminiVertex: false, useAnthropicAws: false, useKimiCoding: false, alwaysOAuthProviderKeys: [], credentialsResetRef: { current: false },
    selectedFetchedModels: [], retryableStatusCodesText: '', retryableErrorPatternsText: '', passThroughUserAgent: null, passThroughBody: null, disableRetries: false, fullPassThrough: false, isAxonHubChannelType: (t) => t === 'axonhub', isAxonHubType: false,
    genericSettingsBeforeAxonHubRef: { current: null }, axonHubCaptureSettings: capture.AXONHUB_CAPTURE_PRESETS.inherit, isOpenCodeGoChannelType: () => false,
    isCodexType: false, isClaudeCodeType: false, authMode: 'official', proxyType: 'disabled', ProxyType: { URL: 'url' }, proxyPresets: [], saveProxyPreset: { mutate: noop },
    t: (s) => s, toast: stubs.sonner.toast, onOpenChange: noop, baseURLMatchesResponsesTransport: () => true,
    createChannel: channels.useCreateChannel(), updateChannel: channels.useUpdateChannel(), duplicateChannel: channels.useDuplicateChannel(),
    supportedModels: [...(initialRow?.supportedModels || ['synthetic'])], manualModels: [], nativeEndpoints: initialRow?.endpoints,
    applyAxonHubChannelTransition: noop, setAuthMode: noop, setUseGeminiVertex: noop, setUseAnthropicAws: noop, setUseKimiCoding: noop, setFetchedModels: noop, setUseFetchedModels: noop,
    setManualModels: (v) => context.manualModels = v, setSupportedModels: (v) => context.supportedModels = v,
    setSelectedProvider: (v) => context.selectedProvider = v, setSelectedApiFormat: (v) => context.selectedApiFormat = v, setNativeEndpoints: (v) => context.nativeEndpoints = v,
  };
  context.form = closure(dialog, 'form', { ...context, useForm: createFormControl, zodResolver,
    formSchema: isEdit ? schema.updateChannelInputSchema : schema.createChannelInputSchema, derivedChannelType: 'openai' });
  context.form.subscribe({ formState: { values: true }, callback: noop });
  context.form.setValue('name', `synthetic-admin-${++serial}`);
  return context;
}
// Declarations used by the real submit closure, not a replacement payload builder.
function declaration(name, context) {
  const tree = ast(dialog); let text;
  tree.forEachChild((node) => { if (ts.isFunctionDeclaration(node) && node.name?.text === name) text = node.getText(tree); });
  assert.ok(text, name); return evaluate(`(function(){ ${text}; return ${name}; })()`, context);
}
async function submit(c) {
  c.derivedChannelType = providers.getChannelTypeForApiFormat(c.selectedProvider, c.selectedApiFormat);
  c.parseRetryableStatusCodesInput = declaration('parseRetryableStatusCodesInput', c);
  c.parseRetryableErrorPatternsInput = declaration('parseRetryableErrorPatternsInput', c);
  const fn = closure(dialog, 'onSubmit', c);
  let invalid = false;
  await c.form.handleSubmit(fn, () => invalid = true)();
  return !invalid;
}
function switchTo(c, provider) { closure(dialog, 'handleProviderChange', c, true)(provider); }
// Creation, editing and duplication all traverse the actual production handler,
// actual RHF state/resolver, actual mutation hooks, HTTP and persisted reread.
const seedInput = { type: 'openai', name: 'synthetic-native-seed', baseURL: native.NATIVE_DECISION_PRESETS.typesafe.baseURL, supportedModels: ['jev-1.14.0'], defaultTestModel: 'jev-1.14.0', credentials: { apiKeys: ['synthetic-typesafe-original'] }, endpoints: native.nativeDecisionEndpoints('typesafe') };
async function savedSource(preset = 'typesafe') {
  const nativePreset = native.isNativeDecisionPreset(preset);
  const input = { ...seedInput, name: `synthetic-source-${++serial}`, ...(nativePreset ? {} : { endpoints: [], baseURL: 'https://api.openai.com/v1' }) };
  return readChannel((await channels.useCreateChannel().mutateAsync(input)).id);
}
const source = await savedSource();
for (const mode of ['create', 'update', 'duplicate']) {
  for (const [from, to] of [['typesafe', 'openai'], ['openai', 'typesafe'], ['typesafe', 'commandcode']]) {
    const initial = mode === 'create' ? undefined : await savedSource(from);
    const c = formCase(mode, initial);
    if (mode === 'create') switchTo(c, from);
    c.form.setValue('credentials', { apiKey: 'synthetic-single-old', apiKeys: ['synthetic-list-old'] });
    switchTo(c, to);
    assert.deepEqual(c.form.getValues('credentials'), { apiKey: undefined, apiKeys: [] });
    const before = mutations.length;
    await submit(c);
    assert.equal(mutations.length, before, `${mode} ${from}->${to} empty credentials sent a mutation`);
    c.form.setValue('credentials.apiKeys', ['synthetic-new-' + to]);
    c.form.setValue('name', `synthetic-admin-${++serial}`);
    await submit(c);
    assert.equal(mutations.length, before + 1, errors.join('\n'));
    const sent = mutations.at(-1);
    assert.deepEqual(sent.variables.input.credentials.apiKeys, ['synthetic-new-' + to]);
    assert.ok(!JSON.stringify(sent.variables).includes('synthetic-list-old'));
    const saved = await readChannel(sent.savedID);
    assert.equal(saved.baseURL, sent.variables.input.baseURL);
    assert.deepEqual(saved.credentials.apiKeys, ['synthetic-new-' + to]);
  }
}
for (const mode of ['update', 'duplicate']) {
  const c = formCase(mode, source);
  if (mode === 'update') c.form.setValue('credentials', { apiKey: '', apiKeys: [] });
  const before = mutations.length;
  await submit(c);
  assert.equal(mutations.length, before + 1, errors.join('\n'));
  if (mode === 'update') assert.equal(mutations.at(-1).variables.input.credentials, undefined);
  const kept = await readChannel(mutations.at(-1).savedID);
  assert.deepEqual(kept.credentials.apiKeys, source.credentials.apiKeys);
  const empty = formCase(mode, source);
  empty.form.setValue('credentials', { apiKey: '', apiKeys: [] });
  const emptyBefore = mutations.length;
  await submit(empty);
  assert.equal(mutations.length, mode === 'update' ? emptyBefore + 1 : emptyBefore);
  const switched = formCase(mode, source);
  switchTo(switched, 'commandcode'); switchTo(switched, 'typesafe');
  const count = mutations.length; await submit(switched); assert.equal(mutations.length, count, 'switch-back resurrected source key');
}
console.log('R1: real RHF -> production create/update/duplicate mutations -> authenticated GraphQL -> persisted reread passed');

const requests = load('features/requests/data/requests.ts');
const evidence = load('features/requests/utils/body-evidence.ts');
const curl = load('features/requests/utils/curl-generator.ts');
const { BodyEvidenceViewer } = load('features/requests/components/body-evidence-viewer.tsx');
const { renderToStaticMarkup } = req('react-dom/server');
const React = req('react');
for (const id of JSON.parse(process.env.AXON_NATIVE_REQUEST_IDS)) {
  const record = await requests.useRequest(id, { disableAutoRefresh: true }).queryFn();
  const executions = await requests.useRequestExecutions(id).queryFn();
  for (const row of [record, ...executions.edges.map((e) => e.node)]) {
    for (const kind of ['requestBody', 'responseBody']) {
      const raw = row[`${kind}Raw`]; assert.equal(typeof raw, 'string');
      assert.equal(raw, JSON.parse(process.env.AXON_NATIVE_EXPECTED_BODIES)[row.id][kind], 'GraphQL evidence differs from the persisted blob');
      assert.ok(raw.includes('9007199254740993'), raw);
      assert.equal(evidence.formatBodyEvidence(row, kind), raw);
      const html = renderToStaticMarkup(React.createElement(BodyEvidenceViewer, { evidence: row, kind }));
      assert.ok(html.includes('9007199254740993'), html);
      assert.ok(!html.includes('9007199254740992'), html);
      assert.match(raw, /[:\[,]\s*9007199254740993\s*[,\]}]/);
      // Parsing is used only for boolean/null type assertions, never for output.
      const typed = JSON.parse(raw);
      if (row.format === 'openai/decisions' && kind === 'responseBody') {
        assert.equal(typed.answers[0].name, null);
        assert.equal(typed.answers[1].choice, true);
        assert.equal(typeof typed.answers[1].choice, 'boolean');
        assert.equal(typeof typed.answers[1].probabilities[1].value, 'string');
      }
    }
    const cmd = row === record ? curl.generateRequestCurl(row.requestHeaders, evidence.bodyEvidence(row, 'requestBody'), row.format) : curl.generateExecutionCurl(row.requestHeaders, evidence.bodyEvidence(row, 'requestBody'), row.channel, row.format, row.requestURL);
    assert.ok(cmd.includes('9007199254740993') && !cmd.includes('9007199254740992'));
  }
  // Execute every production detail button callback for body copy/download/cURL.
  const outputs = [], downloaded = [];
  const detailFile = 'features/requests/components/request-detail-content.tsx';
  const ctx = { ...evidence, ...curl, request: record, execution: executions.edges[0].node, responseView: 'json',
    navigator: { clipboard: { writeText: (s) => outputs.push(s) } }, toast: { success: noop }, t: (s) => s,
    URL: { createObjectURL: (blob) => { downloaded.push(blob); return 'blob:synthetic'; }, revokeObjectURL: noop },
    document: { createElement: () => ({ click: noop }), body: { appendChild: noop, removeChild: noop } },
    setCurlCommand: (s) => outputs.push(s), setShowCurlPreview: noop,
  };
  for (const name of ['copyToClipboard', 'downloadFile']) ctx[name] = closure(detailFile, name, ctx);
  for (const name of ['showRequestCurlPreview', 'showExecutionCurlPreview']) ctx[name] = closure(detailFile, name, ctx, true);
  const tree = ast(detailFile);
  function visit(node) {
    if (ts.isJsxAttribute(node) && node.name.getText(tree) === 'onClick' && node.initializer && ts.isJsxExpression(node.initializer)) {
      const text = node.initializer.expression?.getText(tree) || '';
      if (text.includes('formatBodyEvidence') || text.includes('bodyEvidence')) evaluate(text, ctx)();
    }
    ts.forEachChild(node, visit);
  }
  visit(tree);
  assert.equal(downloaded.length, 4);
  outputs.push(...await Promise.all(downloaded.map((blob) => blob.text())));
  assert.equal(outputs.length, 10);
  for (const [file, callback] of [
    ['features/requests/components/request-body-drawer.tsx', true],
    ['features/channels/components/channels-test-history-drawer.tsx', false],
  ]) {
    const drawerContext = { ...evidence, ...curl, request: record, displayedRequest: record, activeTab: 'request',
      navigator: { clipboard: { writeText: (s) => outputs.push(s) } }, toast: { success: noop }, t: (s) => s,
      setCurlCommand: (s) => outputs.push(s), setShowCurlPreview: noop,
    };
    drawerContext.copyBody = closure(file, 'copyBody', drawerContext, callback);
    closure(file, 'handleCurlPreview', drawerContext, callback)();
    const drawer = ast(file);
    function visitDrawer(node) {
      if (ts.isJsxAttribute(node) && node.name.getText(drawer) === 'onClick' && node.initializer && ts.isJsxExpression(node.initializer)) {
        const text = node.initializer.expression?.getText(drawer) || '';
        if (text.includes('formatBodyEvidence')) {
          for (const activeTab of ['request', 'response']) evaluate(text, { ...drawerContext, activeTab })();
        }
      }
      if (ts.isJsxSelfClosingElement(node) && node.tagName.getText(drawer) === 'BodyEvidenceViewer') {
        const kind = node.attributes.properties.find((p) => p.name?.getText(drawer) === 'kind').initializer.text;
        const html = renderToStaticMarkup(React.createElement(BodyEvidenceViewer, { evidence: record, kind }));
        assert.ok(html.includes('9007199254740993') && !html.includes('9007199254740992'));
      }
      ts.forEachChild(node, visitDrawer);
    }
    visitDrawer(drawer);
  }
  assert.equal(outputs.length, 16, 'all detail and both drawer copy/download/cURL consumers executed');
  for (const text of outputs) assert.ok(text.includes('9007199254740993') && !text.includes('9007199254740992'), text);
}
console.log('R2: production detail/execution queries -> graphqlRequest response.json -> raw viewer and all copy/download/cURL callbacks passed');
