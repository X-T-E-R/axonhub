import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { resolve } from 'node:path';

const root = resolve(import.meta.dirname, '../../frontend');
const require = createRequire(resolve(root, 'package.json'));
const ts = require('typescript');
const source = readFileSync(resolve(root, 'src/features/portal/templates.ts'), 'utf8');
const output = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023 } }).outputText;
const module = { exports: {} };
new Function('module', 'exports', output)(module, module.exports);
const { model, channels } = JSON.parse(readFileSync(0, 'utf8'));
for (const dto of [model, ...channels.flatMap((channel) => channel.models)]) {
  assert.equal(dto.id, 'alias');
  assert.equal(dto.type, 'chat');
  assert.equal(dto.vision, false);
  assert.equal(dto.contextLength, 1024);
  const templates = module.exports.accessTemplates(dto);
  assert.deepEqual(templates.map((template) => template.id), ['responses', 'codex']);
  assert.match(templates[0].code('https://gateway.example.test', dto.id, 'synthetic'), /\/v1\/responses/);
  assert.match(templates[0].code('https://gateway.example.test', dto.id, 'synthetic'), /"model": "alias"/);
}
console.log('Mapped alias: real flat/channel DTOs preserve target metadata and production Responses/Codex templates');
