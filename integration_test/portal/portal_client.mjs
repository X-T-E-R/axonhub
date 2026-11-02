// Production portalRequest, page components and mutation/copy closures against
// the disposable Go JWT/Ent fixture. Rendering is SSR, not browser/visual QA.
import assert from 'node:assert/strict';
import { readFileSync, existsSync, statSync } from 'node:fs';
import { createRequire } from 'node:module';
import { resolve, dirname } from 'node:path';

const root = resolve(import.meta.dirname, '../../frontend/src');
const req = createRequire(resolve(root, '../package.json'));
const ts = req('typescript');
const React = req('react');
const { renderToStaticMarkup } = req('react-dom/server');
const base = process.argv[2];
assert.match(base, /^http:\/\/127\.0\.0\.1:\d+$/);
const userID = process.env.AXON_PORTAL_USER;
const projectID = Number(process.env.AXON_PORTAL_PROJECT);
const token = process.env.AXON_PORTAL_TOKEN;
assert.ok(userID && projectID && token);
globalThis.window = { location: { origin: base } };
const noop = () => {};
const toastErrors = [];
const realFetch = globalThis.fetch;
globalThis.fetch = (url, options) => {
  assert.equal(url, '/portal/graphql', 'portal client must not use an admin endpoint');
  assert.equal(options.headers.Authorization, `Bearer ${token}`);
  return realFetch(base + url, options);
};
const hydrated = new Map();
let group;
const query = (config) => ({ ...config, data: hydrated.get(JSON.stringify(config.queryKey)), refetch: noop });
const stubs = {
  '@tanstack/react-query': { useQuery: query, useMutation: (config) => config, useQueryClient: () => ({ invalidateQueries: noop }) },
  '@tanstack/react-router': { Link: ({ to, search: _search, ...props }) => React.createElement('a', { href: to, ...props }), useNavigate: () => noop, useSearch: () => ({ doc: 'live' }), useRouterState: ({ select }) => select({ location: { search: {} } }) },
  'react-i18next': { useTranslation: () => ({ t: (key) => key, i18n: { language: 'en' } }) },
  // The icon package requires Vite's directory resolution; SVG appearance is not
  // part of this SSR/data-flow check.
  '@lobehub/icons': { OpenAI: (props) => React.createElement('svg', props) },
  sonner: { toast: { error: (message) => toastErrors.push(message), success: noop } },
  '@/stores/authStore': { getTokenFromStorage: () => token, useAuthStore: (select) => select({ auth: { user: { id: userID } } }) },
  '@/gql/graphql': { GraphQLRequestError: class extends Error {} },
};
const layout = {
  usePortalGroup: () => group,
  PortalEmpty: ({ title }) => React.createElement('div', null, title),
  PortalError: ({ error }) => { throw error; },
};
const cache = new Map();
function load(file) {
  let path = resolve(root, file);
  if (!existsSync(path) || statSync(path).isDirectory()) path += existsSync(path + '.ts') ? '.ts' : existsSync(path + '.tsx') ? '.tsx' : '/index.ts';
  if (path === resolve(root, 'features/portal/layout.tsx')) return layout;
  if (cache.has(path)) return cache.get(path).exports;
  const module = { exports: {} }; cache.set(path, module);
  const source = readFileSync(path, 'utf8');
  const output = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023, jsx: ts.JsxEmit.ReactJSX, esModuleInterop: true } }).outputText;
  new Function('require', 'module', 'exports', output)((name) => {
    if (Object.hasOwn(stubs, name)) return stubs[name];
    if (name.startsWith('@/') || name.startsWith('.')) return load(name.startsWith('@/') ? name.slice(2) : resolve(dirname(path), name));
    return req(name);
  }, module, module.exports);
  return module.exports;
}
const data = load('features/portal/data.ts');
const identity = await data.portalRequest(data.IDENTITY_QUERY);
assert.equal(String(identity.me.id), userID);
assert.equal(identity.me.accessMode, 'portal');
group = (await data.portalRequest(data.GROUPS_QUERY)).groups.find((g) => g.id === projectID);
assert.ok(group.member && group.callable);

// Execute the actual KeyEditor save closure; the payload builder is not copied.
function mutation(file, name, context) {
  const path = resolve(root, file);
  const tree = ts.createSourceFile(path, readFileSync(path, 'utf8'), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  let initializer;
  function visit(node) {
    if (ts.isVariableDeclaration(node) && node.name.getText(tree) === name) initializer = node.initializer.getText(tree);
    ts.forEachChild(node, visit);
  }
  visit(tree); assert.ok(initializer);
  return evaluate(initializer, { ...context, useMutation: (config) => config });
}
function evaluate(text, context) {
  const module = { exports: {} };
  const output = ts.transpileModule(`module.exports = ${text};`, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023 } }).outputText;
  new Function(...Object.keys(context), 'module', output)(...Object.values(context), module);
  return module.exports;
}
const editorContext = { ...data, group, item: null, name: 'Node portal key', models: ['m'], ips: '127.0.0.1/32', queryClient: { invalidateQueries: noop }, onClose: noop, toast: stubs.sonner.toast, t: (key) => key };
const created = (await mutation('features/portal/keys.tsx', 'save', editorContext).mutationFn()).createKey;
assert.equal(created.projectID, projectID);
assert.deepEqual(created.modelIDs, ['m']);
assert.deepEqual(created.allowedIPs, ['127.0.0.1/32']);
const updated = (await mutation('features/portal/keys.tsx', 'save', { ...editorContext, item: created, name: 'Node edited key' }).mutationFn()).updateKey;
assert.equal(updated.id, created.id);
assert.equal(updated.name, 'Node edited key');
const statusMutation = mutation('features/portal/keys.tsx', 'action', { ...data, setAfter: noop, queryClient: { invalidateQueries: noop } });
assert.equal((await statusMutation.mutationFn({ id: created.id, status: 'disabled' })).setKeyStatus.status, 'disabled');
assert.equal((await statusMutation.mutationFn({ id: created.id, status: 'enabled' })).setKeyStatus.status, 'enabled');
const rotated = (await statusMutation.mutationFn({ id: created.id, rotate: true })).rotateKey;
assert.equal(rotated.id, created.id);

async function prime(name, source, variables = {}) {
  const config = data.usePortalQuery(name, source, variables);
  hydrated.set(JSON.stringify(config.queryKey), await config.queryFn());
}
await prime('currency', 'query PortalCurrency { currencyCode }');
await prime('content', data.CONTENT_QUERY);
await prime('dashboard', data.DASHBOARD_QUERY, { projectID, period: 'month' });
await prime('keys', data.KEYS_QUERY, { projectID, first: 100 });
await prime('keys', data.KEYS_QUERY, { projectID, first: 20, after: null });
await prime('model-channels', data.MODEL_CHANNELS_QUERY, { projectID, search: '', type: '' });
const keys = await data.portalRequest(data.KEYS_QUERY, { projectID, first: 100 });
const selectedKey = keys.keys.items.find((key) => key.status === 'enabled');
await prime('key-models', data.MODELS_QUERY, { projectID, keyID: selectedKey.id });
const markup = {};
for (const page of ['overview', 'keys', 'models', 'guides']) {
  markup[page] = renderToStaticMarkup(React.createElement(load(`features/portal/${page}.tsx`).default));
  assert.doesNotMatch(markup[page], /synthetic-upstream|B key/);
  if (page !== 'models') assert.doesNotMatch(markup[page], /internal-route/);
}
assert.match(markup.overview, /portal\.overview/);
assert.match(markup.overview, /portal\.modelUsage/);
assert.match(markup.keys, /Node edited key/);
assert.match(markup.models, /portal\.models/);
assert.match(markup.models, /\bm\b/);
assert.match(markup.models, /internal-route/, 'authorized channel display name is shown');
for (const text of Object.values(markup)) assert.doesNotMatch(text, /independentQuota|pricedCoverage|reportedCoverage|modelUsageHelp|nativeNotice/);
assert.match(markup.keys, /apikeys\.columns\.key/);
assert.match(markup.keys, /pagination\.rowsPerPage/);
assert.match(markup.guides, /<h1>Hello<\/h1>/);
assert.doesNotMatch(markup.guides, /secret|Draft/);

// Run QuickStart's real copyCode declaration with its actual model/template data.
const quickFile = resolve(root, 'features/portal/quick-start.tsx');
const tree = ts.createSourceFile(quickFile, readFileSync(quickFile, 'utf8'), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
let declaration;
function findCopy(node) { if (ts.isFunctionDeclaration(node) && node.name?.text === 'copyCode') declaration = node.getText(tree); ts.forEachChild(node, findCopy); }
findCopy(tree); assert.ok(declaration);
const model = (await data.portalRequest(data.MODELS_QUERY, { projectID, keyID: selectedKey.id })).models[0];
const template = load('features/portal/templates.ts').accessTemplates(model)[0];
assert.equal(template.id, 'responses');
let copied;
const copyContext = { ...data, selectedKey, mounted: { current: true }, template, model, base, navigator: { clipboard: { writeText: async (value) => { copied = value; } } }, toast: stubs.sonner.toast, t: (key) => key };
const copy = evaluate(`(function(){ ${declaration}; return copyCode; })()`, copyContext);
await copy();
const revealed = (await data.portalRequest('mutation($id:Int!){revealKey(id:$id)}', { id: selectedKey.id })).revealKey;
assert.ok(copied.includes(revealed));
assert.ok(!JSON.stringify(markup).includes(revealed), 'SSR must keep the secret masked');
copyContext.mounted.current = false;
copied = undefined;
await copy();
assert.equal(copied, undefined, 'an unmounted group must not write a stale secret to clipboard');
assert.deepEqual(toastErrors, []);
console.log('Portal: production request client + four hydrated SSR pages + create/edit/status/rotate/copy closures passed through JWT/GraphQL/Ent; no model calls');
