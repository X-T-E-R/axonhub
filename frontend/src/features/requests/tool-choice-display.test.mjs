import assert from 'node:assert/strict';
import test from 'node:test';
import { formatToolChoiceForDisplay } from './utils/tool-choice.ts';

test('tool choice objects remain readable JSON in the current request viewer', () => {
  assert.deepEqual(formatToolChoiceForDisplay({ type: 'function', function: { name: 'search' } }), {
    kind: 'json',
    value: '{\n  "type": "function",\n  "function": {\n    "name": "search"\n  }\n}',
  });
});

test('tool choice strings remain compact and absent values stay hidden', () => {
  assert.deepEqual(formatToolChoiceForDisplay('auto'), { kind: 'text', value: 'auto' });
  assert.equal(formatToolChoiceForDisplay(null), null);
});
