import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, extname, join, resolve } from 'node:path';
import test from 'node:test';

const sourceRoot = resolve(import.meta.dirname, '../..');
const frontendRoot = resolve(sourceRoot, '..');
const nativeRequire = createRequire(join(frontendRoot, 'package.json'));
const ts = nativeRequire('typescript');
const moduleCache = new Map();

function withSourceExtension(path) {
  if (existsSync(path) && ['.ts', '.tsx'].includes(extname(path))) return path;
  for (const extension of ['.ts', '.tsx']) {
    if (existsSync(path + extension)) return path + extension;
  }
  for (const extension of ['.ts', '.tsx']) {
    const index = join(path, 'index' + extension);
    if (existsSync(index)) return index;
  }
  throw new Error(`Cannot resolve local TypeScript module: ${path}`);
}

function resolveLocal(fromFile, request) {
  return withSourceExtension(request.startsWith('@/') ? join(sourceRoot, request.slice(2)) : resolve(dirname(fromFile), request));
}

// This is intentionally the same in-memory TypeScript loader shape as the existing
// frontend/src/features/native-decisions.test.mjs. It emits no bundle and writes no generated file.
function load(relativeOrAbsolutePath) {
  const path = withSourceExtension(
    relativeOrAbsolutePath.startsWith('/') || /^[A-Za-z]:[\\/]/.test(relativeOrAbsolutePath)
      ? relativeOrAbsolutePath
      : join(sourceRoot, relativeOrAbsolutePath)
  );
  if (moduleCache.has(path)) return moduleCache.get(path).exports;

  const source = readFileSync(path, 'utf8');
  const output = ts.transpileModule(source, {
    compilerOptions: {
      jsx: ts.JsxEmit.ReactJSX,
      module: ts.ModuleKind.CommonJS,
      moduleResolution: ts.ModuleResolutionKind.NodeJs,
      target: ts.ScriptTarget.ES2023,
      esModuleInterop: true,
    },
    fileName: path,
  });
  const module = { exports: {} };
  moduleCache.set(path, module);
  const localRequire = (request) => {
    if (request.startsWith('@/') || request.startsWith('.')) return load(resolveLocal(path, request));
    return nativeRequire(request);
  };
  new Function('require', 'module', 'exports', output.outputText)(localRequire, module, module.exports);
  return module.exports;
}

const React = nativeRequire('react');
const { renderToStaticMarkup } = nativeRequire('react-dom/server');

// Keep the real CodeBlock/CodeBlockCopyButton implementation, but observe the
// props that PortalMarkdown sends into it. This makes the language assertion
// exercise the production markdown parser and avoids reimplementing its lookup.
const buttonModule = load('components/ui/button.tsx');
const originalButton = buttonModule.Button;
let observedButtonProps;
buttonModule.Button = (props) => {
  observedButtonProps = props;
  return originalButton(props);
};
const codeBlockModule = load('components/ai-elements/code-block.tsx');
const originalCodeBlock = codeBlockModule.CodeBlock;
const observedCodeBlocks = [];
codeBlockModule.CodeBlock = (props) => {
  observedCodeBlocks.push(props);
  return React.createElement(originalCodeBlock, props);
};

const { PortalMarkdown, safePortalURL } = load('features/portal/markdown.tsx');
const { accessTemplates } = load('features/portal/templates.ts');
const { highlightCode } = codeBlockModule;
const quickStartSource = readFileSync(join(sourceRoot, 'features/portal/quick-start.tsx'), 'utf8');
const portalKeysPath = join(sourceRoot, 'features/portal/keys.tsx');
const portalKeysSource = readFileSync(portalKeysPath, 'utf8');

function renderMarkdown(markdown) {
  return renderToStaticMarkup(React.createElement(PortalMarkdown, { children: markdown }));
}

function ids(model) {
  return accessTemplates(model).map((template) => template.id);
}

function model(type, apiFormats) {
  return { type, apiFormats };
}

function templateFor(type, apiFormats, id = undefined) {
  const templates = accessTemplates(model(type, apiFormats));
  const template = id === undefined ? templates[0] : templates.find((candidate) => candidate.id === id);
  assert.ok(template, `expected ${type} template ${id || '(first)'}`);
  return template;
}

function parseCurlCommand(command) {
  const url = command.match(/^curl '([^']+)'/)?.[1];
  const bodyText = command.match(/\n  -d '([\s\S]*)'$/)?.[1];
  assert.ok(url, `expected a quoted curl URL in ${command}`);
  assert.ok(bodyText, `expected a quoted JSON curl body in ${command}`);
  return { url, body: JSON.parse(bodyText) };
}

function extractCopyKey({ portalRequest, navigator, toast, t }) {
  const sourceFile = ts.createSourceFile(portalKeysPath, portalKeysSource, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  let declaration;
  function visit(node) {
    if (ts.isFunctionDeclaration(node) && node.name?.text === 'copyKey') declaration = node.getText(sourceFile);
    ts.forEachChild(node, visit);
  }
  visit(sourceFile);
  assert.ok(declaration, 'keys.tsx copyKey function should remain extractable for the counterexample harness');
  const module = { exports: {} };
  const output = ts.transpileModule(
    `module.exports = (function () { const mounted = { current: true }; ${declaration}; return { copyKey, mounted }; })();`,
    { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023 } }
  ).outputText;
  new Function('module', 'portalRequest', 'navigator', 'toast', 't', output)(module, portalRequest, navigator, toast, t);
  return module.exports;
}

test('portal markdown blocks raw HTML and dangerous protocols while securing normal external links', () => {
  const output = renderMarkdown(
    [
      '<script>alert(1)</script>',
      '<img src=x onerror="alert(2)">',
      '<div onerror="alert(3)">unsafe html</div>',
      '[javascript](javascript:alert(4))',
      '[vbscript](vbscript:msgbox(5))',
      '[data](data:text/html,<script>alert(6)</script>)',
      '[safe](https://docs.example.test/guide)',
    ].join('\n\n')
  );

  assert.doesNotMatch(output, /<script\b/i);
  assert.doesNotMatch(output, /<img\b/i);
  assert.doesNotMatch(output, /onerror\s*=/i);
  assert.doesNotMatch(output, /href="(?:javascript|vbscript|data):/i);
  assert.match(output, /<a href="https:\/\/docs\.example\.test\/guide" target="_blank" rel="noopener noreferrer">safe<\/a>/);
  for (const unsafe of ['javascript:', 'vbscript:', 'data:text/html']) assert.doesNotMatch(output, new RegExp(unsafe, 'i'));

  assert.equal(safePortalURL('https://docs.example.test/guide'), 'https://docs.example.test/guide');
  assert.equal(safePortalURL('javascript:alert(1)'), '');
  assert.equal(safePortalURL('java\u0000script:alert(1)'), '');
  assert.equal(safePortalURL('data:text/html,<script>alert(1)</script>'), '');
});

test('real PortalMarkdown routes an unknown fenced language to CodeBlock text safely', async () => {
  observedCodeBlocks.length = 0;
  observedButtonProps = undefined;
  const source = 'const safe = true;';
  const output = renderMarkdown('```toString\n' + source + '\n```');
  assert.match(output, /<button[^>]*aria-label="Copy code"/);
  assert.equal(observedCodeBlocks.length, 1, 'PortalMarkdown should invoke the real CodeBlock once');
  const block = observedCodeBlocks[0];
  assert.equal(block.language, 'text', 'unknown prototype language must be selected as text by production markdown');
  assert.equal(block.code, source);
  await assert.doesNotReject(
    () => highlightCode(block.code, block.language),
    'the language selected by real PortalMarkdown must be accepted by real Shiki highlighting'
  );
});

test('real CodeBlockCopyButton onClick copies the CodeBlock source through a mocked clipboard', async () => {
  observedCodeBlocks.length = 0;
  observedButtonProps = undefined;
  const output = renderMarkdown('```javascript\nconst copied = 42;\n```');
  assert.match(output, /<button[^>]*aria-label="Copy code"/);
  assert.equal(observedCodeBlocks.length, 1);
  assert.equal(typeof observedButtonProps?.onClick, 'function', 'real CodeBlockCopyButton should provide an onClick handler');

  let copied;
  const originalWindowDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'window');
  const originalNavigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  const originalSetTimeout = globalThis.setTimeout;
  Object.defineProperty(globalThis, 'window', { configurable: true, writable: true, value: {} });
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    writable: true,
    value: {
      clipboard: {
        writeText: async (value) => {
          copied = value;
        },
      },
    },
  });
  globalThis.setTimeout = (callback) => {
    callback();
    return 0;
  };
  try {
    await observedButtonProps.onClick();
  } finally {
    globalThis.setTimeout = originalSetTimeout;
    if (originalWindowDescriptor) Object.defineProperty(globalThis, 'window', originalWindowDescriptor);
    else delete globalThis.window;
    if (originalNavigatorDescriptor) Object.defineProperty(globalThis, 'navigator', originalNavigatorDescriptor);
    else delete globalThis.navigator;
  }

  assert.equal(copied, observedCodeBlocks[0].code, 'clipboard should receive the exact source held by CodeBlock context');
});

// Execute the production copy closure with explicit lifecycle and clipboard stubs.
test('keys copyKey reveals and writes the active group secret exactly once', async () => {
  const writes = [];
  let successCount = 0;
  const { copyKey, mounted } = extractCopyKey({
    portalRequest: async () => ({ revealKey: 'ah-synthetic-active-group-secret' }),
    navigator: { clipboard: { writeText: async (value) => writes.push(value) } },
    toast: {
      success: () => {
        successCount += 1;
      },
      error: () => assert.fail('active copy should not report an error'),
    },
    t: (key) => key,
  });

  await copyKey(42);
  assert.equal(mounted.current, true);
  assert.deepEqual(writes, ['ah-synthetic-active-group-secret']);
  assert.equal(successCount, 1);
  assert.match(quickStartSource, /const mounted = useRef\(true\)/);
  assert.match(quickStartSource, /if \(!mounted\.current \|\| !template \|\| !model\) return;/);
});

test('keys copyKey drops a delayed reveal after the group subtree unmounts', async () => {
  let resolveRequest;
  const request = new Promise((resolve) => {
    resolveRequest = resolve;
  });
  const writes = [];
  let successCount = 0;
  const { copyKey, mounted } = extractCopyKey({
    portalRequest: () => request,
    navigator: { clipboard: { writeText: async (value) => writes.push(value) } },
    toast: {
      success: () => {
        successCount += 1;
      },
      error: () => assert.fail('stale copy should not report an error'),
    },
    t: (key) => key,
  });

  const pending = copyKey(42);
  mounted.current = false;
  resolveRequest({ revealKey: 'ah-synthetic-old-group-secret' });
  await pending;

  assert.deepEqual(writes, [], 'a delayed reveal from an unmounted group must not reach clipboard');
  assert.equal(successCount, 0);
});

test('Codex is emitted only for Responses-capable chat models', () => {
  assert.deepEqual(ids(model('chat', ['openai/chat_completions'])), ['chat']);
  assert.deepEqual(ids(model('chat', ['openai/responses'])), ['responses', 'codex']);
  assert.deepEqual(ids(model('chat', ['openai/chat_completions', 'openai/responses'])), ['chat', 'responses', 'codex']);
  assert.deepEqual(ids(model('chat', ['typesafe/systemone'])), ['systemone']);
  assert.deepEqual(ids(model('chat', ['openai/decisions'])), ['decisions']);

  const codex = templateFor('chat', ['openai/responses'], 'codex');
  const codexCode = codex.code('https://axon.invalid', 'gpt-responses', 'secret');
  assert.match(codexCode, /wire_api = "responses"/);
  assert.doesNotMatch(codexCode, /chat_completions/);
});

test('native decisions and System One templates emit their expected wire shapes', () => {
  const systemone = parseCurlCommand(
    templateFor('chat', ['typesafe/systemone'], 'systemone').code('https://axon.invalid', 'native-model', 'secret')
  );
  assert.equal(systemone.url, 'https://axon.invalid/v1/systemone');
  assert.deepEqual(systemone.body, {
    model: 'native-model',
    state: 'This is a duplicate charge.',
    questions: { duplicate: { type: 'noul', instructions: 'Is this a duplicate charge?' } },
  });

  const commandcode = parseCurlCommand(
    templateFor('chat', ['commandcode/systemone'], 'systemone').code('https://axon.invalid', 'command-model', 'secret')
  );
  assert.equal(commandcode.url, 'https://axon.invalid/v1/systemone');
  assert.equal(commandcode.body.model, 'command-model');
  assert.deepEqual(commandcode.body.questions, systemone.body.questions);

  const decisions = parseCurlCommand(
    templateFor('chat', ['openai/decisions'], 'decisions').code('https://axon.invalid', 'decision-model', 'secret')
  );
  assert.equal(decisions.url, 'https://axon.invalid/v1/decisions');
  assert.deepEqual(decisions.body, {
    model: 'decision-model',
    input: 'This is a duplicate charge.',
    questions: [{ type: 'predicate', instructions: 'Is this a duplicate charge?' }],
  });
});

test('Jina embedding and Seedance native templates use their real endpoint and body contracts', () => {
  const jina = parseCurlCommand(
    templateFor('embedding', ['jina/embeddings'], 'embedding').code('https://axon.invalid', 'jina-embed', 'secret')
  );
  assert.equal(jina.url, 'https://axon.invalid/jina/v1/embeddings');
  assert.deepEqual(jina.body, { model: 'jina-embed', input: 'Hello' });

  const seedance = parseCurlCommand(
    templateFor('video_generation', ['seedance/video'], 'video').code('https://axon.invalid', 'seedance-video', 'secret')
  );
  assert.equal(seedance.url, 'https://axon.invalid/doubao/v3/contents/generations/tasks');
  assert.equal(seedance.body.model, 'seedance-video');
  assert.ok(Array.isArray(seedance.body.content));
  assert.deepEqual(seedance.body.content, [{ type: 'text', text: 'A quiet mountain lake' }]);
});

test('native-only and modality-specific protocols get dedicated templates', () => {
  assert.deepEqual(ids(model('chat', ['typesafe/systemone'])), ['systemone']);
  assert.deepEqual(ids(model('chat', ['commandcode/systemone'])), ['systemone']);
  assert.deepEqual(ids(model('chat', ['openai/decisions'])), ['decisions']);
  assert.deepEqual(ids(model('embedding', ['openai/embeddings'])), ['embedding']);
  assert.deepEqual(ids(model('rerank', ['jina/rerank'])), ['rerank']);
  assert.deepEqual(ids(model('image_generation', ['openai/image_generation'])), ['image']);
  assert.deepEqual(ids(model('video_generation', ['openai/video'])), ['video']);
  assert.equal(
    templateFor('chat', ['typesafe/systemone'], 'systemone')
      .code('https://axon.invalid', 'native-model', 'secret')
      .includes('/v1/systemone'),
    true
  );
  assert.equal(
    templateFor('chat', ['openai/decisions'], 'decisions').code('https://axon.invalid', 'native-model', 'secret').includes('/v1/decisions'),
    true
  );
});

test('models with no compatible protocol receive no template', () => {
  assert.deepEqual(ids(model('chat', ['openai/audio'])), []);
  assert.deepEqual(ids(model('embedding', ['openai/chat_completions'])), []);
  assert.deepEqual(ids(model('rerank', ['openai/responses'])), []);
  assert.deepEqual(ids(model('image_generation', ['openai/responses'])), []);
  assert.deepEqual(ids(model('video_generation', ['openai/chat_completions'])), []);
  assert.match(quickStartSource, /const templates = model \? accessTemplates\(model\) : \[\];/);
  assert.match(quickStartSource, /templates\.length > 0 \?/);
});
