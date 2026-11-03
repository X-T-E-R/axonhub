import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { resolve } from 'node:path';
import test from 'node:test';

const require = createRequire(import.meta.url);
const ts = require('typescript');
const React = require('react');
const { renderToStaticMarkup } = require('react-dom/server');
const path = resolve(import.meta.dirname, 'components/channels-keys-dialog.tsx');
const source = readFileSync(path, 'utf8');
const tree = ts.createSourceFile(path, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const controls = { real_request: [], balance_probe: [] };
let realRequestCondition;

// Exercise the production JSX control fragments, with UI wrappers and mutation
// delivery explicitly stubbed. This is a runtime visibility/callback test, not a
// browser screenshot or a replacement implementation of the dialog.
function visit(node) {
  if (ts.isVariableDeclaration(node) && node.name.getText(tree) === 'canRunRealRequest') {
    realRequestCondition = node.initializer.getText(tree);
  }
  if (ts.isJsxElement(node) && node.openingElement.tagName.getText(tree) === 'Button') {
    const onClick = node.openingElement.attributes.properties.find(
      (attribute) => ts.isJsxAttribute(attribute) && attribute.name.getText(tree) === 'onClick'
    );
    const callback = onClick?.initializer?.expression;
    if (callback && ts.isArrowFunction(callback) && ts.isCallExpression(callback.body) && callback.body.expression.getText(tree) === 'handleRunChecks') {
      const mode = callback.body.arguments[1]?.text;
      if (mode in controls) {
        let parent = node.parent;
        while (parent && !ts.isJsxExpression(parent)) parent = parent.parent;
        assert.ok(parent?.expression, `${mode} button should be inside a visibility expression`);
        controls[mode].push(parent.expression.getText(tree));
      }
    }
  }
  ts.forEachChild(node, visit);
}
visit(tree);

function renderControls(mode, currentRow, canRunBalanceProbe = false) {
  const callbacks = [];
  const calls = [];
  const Button = ({ children, onClick, disabled, ...props }) => {
    callbacks.push(onClick);
    return React.createElement('button', { disabled, 'aria-label': props['aria-label'] }, children);
  };
  const Pass = ({ children }) => React.createElement(React.Fragment, null, children);
  const Icon = () => null;
  const jsx = ts.transpileModule(`module.exports = [${controls[mode].join(',')}];`, {
    compilerOptions: { jsx: ts.JsxEmit.ReactJSX, module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023 },
    fileName: 'raw-health-controls.tsx',
  }).outputText;
  const canRunRealRequest = new Function('currentRow', `return ${realRequestCondition};`)(currentRow);
  const module = { exports: {} };
  const bindings = {
    require, module, exports: module.exports, Button, Tooltip: Pass, TooltipTrigger: Pass, TooltipContent: Pass,
    IconPlayerPlay: Icon, IconChartLine: Icon, t: (key) => key,
    canRunRealRequest, canRunBalanceProbe, isPending: false, inventory: [{}],
    selectedHealthCheckKeyIDs: ['key-1'], item: { id: 'key-1', status: 'active' },
    handleRunChecks: (ids, checkMode) => calls.push({ ids, mode: checkMode }),
  };
  new Function(...Object.keys(bindings), jsx)(...Object.values(bindings));
  const markup = renderToStaticMarkup(React.createElement(React.Fragment, null, ...module.exports));
  for (const callback of callbacks) callback();
  return { markup, calls };
}

test('raw keys remove all three built-in chat checks; legacy keys keep their callbacks', () => {
  assert.equal(controls.real_request.length, 3, 'all, selected, and row checks are covered');
  const raw = renderControls('real_request', { settings: { rawHTTP: { enabled: true } } });
  assert.equal(raw.markup, '');
  assert.deepEqual(raw.calls, []);
  const legacy = renderControls('real_request', { settings: {} });
  assert.equal((legacy.markup.match(/<button/g) ?? []).length, 3);
  assert.deepEqual(legacy.calls, [
    { ids: undefined, mode: 'real_request' },
    { ids: ['key-1'], mode: 'real_request' },
    { ids: ['key-1'], mode: 'real_request' },
  ]);
});

test('raw keys retain all three explicitly configured balance probe controls', () => {
  assert.equal(controls.balance_probe.length, 3);
  const raw = { settings: { rawHTTP: { enabled: true } } };
  const configured = renderControls('balance_probe', raw, true);
  assert.equal((configured.markup.match(/<button/g) ?? []).length, 3);
  assert.deepEqual(configured.calls.map((call) => call.mode), ['balance_probe', 'balance_probe', 'balance_probe']);
  const absent = renderControls('balance_probe', raw, false);
  assert.equal(absent.markup, '');
  assert.deepEqual(absent.calls, []);
});
