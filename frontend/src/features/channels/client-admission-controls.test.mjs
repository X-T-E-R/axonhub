import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import test from 'node:test';

const require = createRequire(import.meta.url);
const ts = require('typescript');
const React = require('react');
const { renderToStaticMarkup } = require('react-dom/server');
const path = new URL('./components/channels-action-dialog.tsx', import.meta.url);
const source = readFileSync(path, 'utf8');
const tree = ts.createSourceFile(path.pathname, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
let fragment;
function visit(node) {
  if (ts.isCallExpression(node) && ts.isPropertyAccessExpression(node.expression) && node.expression.name.text === 'map') {
    const text = node.getText(tree);
    if (text.startsWith("(['codexOfficialClient', 'denyCodexOfficialClient']")) fragment = text;
  }
  ts.forEachChild(node, visit);
}
visit(tree);
assert.ok(fragment, 'both policy controls must be rendered by the real channel form');
const js = ts.transpileModule(`module.exports = ${fragment};`, {
  compilerOptions: { jsx: ts.JsxEmit.ReactJSX, module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023 },
  fileName: 'admission-controls.tsx',
}).outputText;

function render(policies) {
  const callbacks = {};
  const controls = {};
  const Pass = ({ children }) => React.createElement(React.Fragment, null, children);
  const form = {
    control: {},
    setValue: (path, value) => { policies[path.split('.')[1]] = value; },
  };
  let current;
  const FormField = ({ name, render }) => {
    current = name.split('.')[1];
    return render({ field: { value: policies[current], onChange: (value) => { policies[name.split('.')[1]] = value; } } });
  };
  const Checkbox = ({ checked, onCheckedChange }) => {
    controls[current] = checked;
    callbacks[current] = onCheckedChange;
    return React.createElement('input', { type: 'checkbox', checked, readOnly: true });
  };
  const module = { exports: {} };
  const bindings = { require, module, exports: module.exports, form, FormField, Checkbox, FormItem: Pass, FormLabel: Pass, FormControl: Pass, t: (key) => key };
  new Function(...Object.keys(bindings), js)(...Object.values(bindings));
  renderToStaticMarkup(React.createElement(React.Fragment, null, ...module.exports));
  return { callbacks, controls };
}

test('Codex require and deny controls echo saved state and remain mutually exclusive', () => {
  const policies = { stream: 'unlimited', codexOfficialClient: true, denyCodexOfficialClient: false };
  let state = render(policies);
  assert.deepEqual(state.controls, { codexOfficialClient: true, denyCodexOfficialClient: false });
  state.callbacks.denyCodexOfficialClient(true);
  assert.deepEqual(policies, { stream: 'unlimited', codexOfficialClient: false, denyCodexOfficialClient: true });
  state = render(policies);
  assert.equal(state.controls.denyCodexOfficialClient, true);
  state.callbacks.codexOfficialClient(true);
  assert.deepEqual(policies, { stream: 'unlimited', codexOfficialClient: true, denyCodexOfficialClient: false });
});

test('an untouched normal channel does not acquire a deny policy', () => {
  const policies = { stream: 'unlimited', codexOfficialClient: false };
  assert.deepEqual(render(policies).controls, { codexOfficialClient: false, denyCodexOfficialClient: false });
  assert.equal(policies.denyCodexOfficialClient, undefined);
});

test('all channel policy query projections read the deny field and locale keys agree', () => {
  const data = readFileSync(new URL('./data/channels.ts', import.meta.url), 'utf8');
  assert.equal((data.match(/codexOfficialClient\s+denyCodexOfficialClient/g) ?? []).length, 6);
  for (const locale of ['en', 'zh-CN']) {
    const strings = JSON.parse(readFileSync(new URL(`../../locales/${locale}/channels.json`, import.meta.url), 'utf8'));
    for (const policy of ['codexOfficialClient', 'denyCodexOfficialClient']) {
      for (const key of ['label', 'description']) assert.ok(strings[`channels.dialogs.fields.${policy}.${key}`]);
    }
  }
});
