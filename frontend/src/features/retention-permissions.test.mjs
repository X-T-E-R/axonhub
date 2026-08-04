import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

test('trace and thread retention mutations require write_requests', () => {
  const detailPages = [
    'features/threads/components/thread-detail-page.tsx',
    'features/traces/components/trace-detail-page.tsx',
  ];

  for (const path of detailPages) {
    const source = read(path);
    assert.match(source, /const canWriteRequests = hasScope\('write_requests'\)/, `${path} should resolve write access`);
    assert.match(source, /\{canWriteRequests && \(\(\) => \{/, `${path} should hide retention buttons from read-only users`);
    assert.match(source, /open=\{canWriteRequests && showArchiveDialog\}/, `${path} should keep the archive dialog closed for read-only users`);
  }

  const tableActions = [
    'features/threads/components/threads-columns.tsx',
    'features/traces/components/traces-columns.tsx',
    'features/threads/components/threads-row-actions.tsx',
    'features/traces/components/traces-row-actions.tsx',
  ];

  for (const path of tableActions) {
    const source = read(path);
    assert.match(source, /if \(!hasScope\('write_requests'\)\) \{\s*return null;/, `${path} should omit retention actions without write access`);
  }
});
