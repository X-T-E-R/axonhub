// Production registration/admin clients and form mutation closures against a
// disposable JWT/GraphQL/Ent fixture. No browser and no provider calls.
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
const adminToken = process.env.AXON_REGISTRATION_ADMIN;
const projectID = Number(process.env.AXON_REGISTRATION_GROUP);
assert.ok(adminToken && projectID);
let token = adminToken;
let storedUser;
let destination;
const noop = () => {};
const state = { auth: { user: {}, setUser: (u) => { storedUser = u; }, setAccessToken: (value) => { token = value; } } };
const useAuthStore = (selector) => selector(state);
useAuthStore.getState = () => state;
const hydrated = new Map();
const realFetch = globalThis.fetch;
globalThis.window = { location: { origin: base } };
globalThis.fetch = (url, options) => {
  assert.match(url, /^\/(admin\/(auth\/(registration|signup|signin)|graphql)|portal\/graphql)$/);
  return realFetch(base + url, options);
};
const stubs = {
  '@tanstack/react-query': { useQuery: (config) => ({ ...config, data: hydrated.get(JSON.stringify(config.queryKey)), refetch: noop }), useMutation: (config) => config, useQueryClient: () => ({ setQueryData: noop, invalidateQueries: noop }) },
  '@tanstack/react-router': { Link: ({ to, ...props }) => React.createElement('a', { href: to, ...props }), useRouter: () => ({ navigate: ({ to }) => { destination = to; } }), useNavigate: () => noop },
  'react-i18next': { useTranslation: () => ({ t: (key) => key, i18n: { language: 'en' } }) },
  '@/lib/i18n': { __esModule: true, default: { t: (key) => key, language: 'en', changeLanguage: noop } },
  '@/stores/authStore': { getTokenFromStorage: () => token, setTokenToStorage: (value) => { token = value; }, removeTokenFromStorage: noop, useAuthStore },
  '@/hooks/usePermissions': { usePermissions: () => ({ hasSystemScope: () => true }) },
  sonner: { toast: { success: noop, error: noop } },
};
const cache = new Map();
function load(file) {
  let path = resolve(root, file);
  if (!existsSync(path) || statSync(path).isDirectory()) path += existsSync(path + '.ts') ? '.ts' : existsSync(path + '.tsx') ? '.tsx' : '/index.ts';
  const portable = path.replaceAll('\\', '/');
  if (portable.endsWith('login-styles.css')) return {};
  if (portable.endsWith('/auth-layout.tsx') || portable.endsWith('/two-column-auth.tsx')) return { __esModule: true, default: ({ children }) => React.createElement('div', null, children) };
  if (portable.endsWith('/animated-line-background.tsx')) return { __esModule: true, default: () => null };
  if (portable.endsWith('/sign-in/components/user-auth-form.tsx')) return { UserAuthForm: () => React.createElement('form') };
  if (cache.has(path)) return cache.get(path).exports;
  const module = { exports: {} }; cache.set(path, module);
  const output = ts.transpileModule(readFileSync(path, 'utf8'), { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023, jsx: ts.JsxEmit.ReactJSX, esModuleInterop: true } }).outputText;
  const exposed = portable.endsWith('/users/components/users-invite-dialog.tsx') ? output + '\nmodule.exports.testInviteManager = InviteManager;' : output;
  new Function('require', 'module', 'exports', exposed)((name) => {
    if (Object.hasOwn(stubs, name)) return stubs[name];
    if (name.startsWith('@/') || name.startsWith('.')) return load(name.startsWith('@/') ? name.slice(2) : resolve(dirname(path), name));
    return req(name);
  }, module, module.exports);
  return module.exports;
}
function evaluate(text, context) {
  const module = { exports: {} };
  const code = ts.transpileModule(`module.exports = ${text};`, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023 } }).outputText;
  new Function(...Object.keys(context), 'module', code)(...Object.values(context), module);
  return module.exports;
}
function mutation(file, name, context) {
  const path = resolve(root, file);
  const tree = ts.createSourceFile(path, readFileSync(path, 'utf8'), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  let initializer;
  function visit(node) { if (ts.isVariableDeclaration(node) && node.name.getText(tree) === name) initializer = node.initializer.getText(tree); ts.forEachChild(node, visit); }
  visit(tree); assert.ok(initializer, name);
  return evaluate(initializer, { ...context, useMutation: (config) => config });
}
const { authApi } = load('lib/api-client.ts');
const { graphqlRequest } = load('gql/graphql.ts');
const adminData = load('features/users/data/registration.ts');
const statusHook = load('features/auth/data/registration.ts');
async function hydrateStatus() {
  const config = statusHook.useRegistrationStatus();
  const result = await config.queryFn();
  hydrated.set(JSON.stringify(config.queryKey), result);
  return result;
}
await graphqlRequest('mutation{setInvitationRegistration(enabled:false)}');
assert.equal((await hydrateStatus()).enabled, false);
const signin = load('features/auth/sign-in/index.tsx').default;
const signup = load('features/auth/sign-up/index.tsx').default;
assert.doesNotMatch(renderToStaticMarkup(React.createElement(signin)), /href="\/sign-up"/);
assert.match(renderToStaticMarkup(React.createElement(signup)), /registration\.closed/);
const switchMutation = mutation('features/system/components/registration-settings.tsx', 'update', { graphqlRequest, queryClient: { setQueryData: noop } });
await switchMutation.mutationFn(true);
assert.equal((await hydrateStatus()).enabled, true);
assert.match(renderToStaticMarkup(React.createElement(signin)), /href="\/sign-up"/);
const formMarkup = renderToStaticMarkup(React.createElement(signup));
for (const name of ['firstName', 'lastName', 'email', 'password', 'confirmPassword', 'inviteCode']) assert.match(formMarkup, new RegExp(`name="${name}"`));
assert.doesNotMatch(formMarkup, /name="(projectID|roles|scopes|accessMode)"/);

let generatedCode;
const create = mutation('features/users/components/users-invite-dialog.tsx', 'create', { ...adminData, graphqlRequest, projectID: String(projectID), maxUses: 2, expiresAt: '', setCode: (value) => { generatedCode = value; }, query: { refetch: noop } });
assert.equal(await create.mutationFn(), undefined, 'plaintext must not become React Query mutation data');
assert.match(generatedCode, /^axinv_[A-Za-z0-9_-]{43}$/);
const listing = await graphqlRequest(adminData.INVITES_QUERY);
assert.ok(listing.registrationGroups.some((g) => g.id === projectID));
const invite = listing.registrationInvites[0];
assert.equal(invite.projectID, projectID);
assert.equal(invite.maxUses, 2);
assert.ok(!JSON.stringify(listing).includes(generatedCode));
hydrated.set(JSON.stringify(['registration-invites']), listing);
const managerMarkup = renderToStaticMarkup(React.createElement(load('features/users/components/users-invite-dialog.tsx').testInviteManager));
for (const id of ['invite-group', 'invite-uses', 'invite-expiry']) assert.match(managerMarkup, new RegExp(`id="${id}"`));
assert.match(managerMarkup, /Registration group/);
assert.match(managerMarkup, /registration\.valid/);
assert.match(managerMarkup, /max="10000"/);
assert.ok(!managerMarkup.includes(generatedCode));

const productionSignIn = load('features/auth/data/auth.ts').useSignIn();
const signIn = { mutateAsync: async (data) => { const signed = await productionSignIn.mutationFn(data); productionSignIn.onSuccess(signed); return signed; } };
const register = mutation('features/auth/sign-up/components/sign-up-form.tsx', 'register', { authApi, signIn });
const input = { email: 'node-registration@example.test', password: 'SyntheticPassword1', confirmPassword: 'SyntheticPassword1', firstName: 'Node', lastName: 'User', inviteCode: generatedCode };
const registered = await register.mutationFn(input);
assert.equal(registered.success, true);
await register.onSuccess(registered, input);
assert.equal(destination, '/portal');
assert.equal(storedUser.accessMode, 'portal');
assert.equal(storedUser.isOwner, false);
assert.deepEqual(storedUser.scopes, []);
const portal = load('features/portal/data.ts');
const groups = (await portal.portalRequest(portal.GROUPS_QUERY)).groups;
assert.equal(groups.length, 1);
assert.equal(groups[0].id, projectID);
const models = (await portal.portalRequest(portal.MODEL_CHANNELS_QUERY, { projectID })).modelChannels;
assert.equal(models[0].models[0].id, 'm');
const createdKey = await portal.portalRequest(`mutation($projectID:Int!){createKey(projectID:$projectID,input:{name:"Registration client key"}){id projectID status}}`, { projectID });
assert.equal(createdKey.createKey.projectID, projectID);
assert.equal(createdKey.createKey.status, 'enabled');
await assert.rejects(() => graphqlRequest(adminData.INVITES_QUERY), /Forbidden/);
token = adminToken;
const revoke = mutation('features/users/components/users-invite-dialog.tsx', 'revoke', { ...adminData, graphqlRequest, query: { refetch: noop } });
await revoke.mutationFn(invite.id);
await assert.rejects(() => authApi.signUp({ ...input, email: 'revoked-node@example.test' }), /invitation is invalid/);
assert.equal((await graphqlRequest(adminData.INVITES_QUERY)).registrationInvites.find((i) => i.id === invite.id).usedUses, 1);
console.log('Registration: production switch/generate/revoke + form submit + real sign-in/JWT portal navigation/models/own Key passed; closed/open entry rendered, plaintext absent from list and mutation result');
