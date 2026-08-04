import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const frontendRoot = join(import.meta.dirname, '..', '..');
const repositoryRoot = join(frontendRoot, '..');

function readFrontend(relativePath) {
  return readFileSync(join(frontendRoot, relativePath), 'utf8');
}

test('currency formatting supplies a concrete currency before Intl.NumberFormat', () => {
  assert.throws(
    () => new Intl.NumberFormat('en-US', { style: 'currency', currency: undefined }).format(1),
    (error) => error instanceof TypeError || error instanceof RangeError,
    'the original undefined-currency path must remain a demonstrated failure'
  );

  for (const path of [
    'src/features/requests/components/request-detail-content.tsx',
    'src/features/requests/components/requests-columns.tsx',
    'src/features/threads/components/thread-detail-page.tsx',
    'src/features/traces/components/trace-detail-page.tsx',
  ]) {
    assert.match(readFrontend(path), /currency:\s*settings\?\.currencyCode \?\? 'USD'/, `${path} should supply the USD fallback`);
  }

  const i18n = readFrontend('src/lib/i18n.ts');
  const numberFormat = i18n.slice(i18n.indexOf('new Intl.NumberFormat'), i18n.indexOf('}).format(value)') + 2);
  assert.doesNotMatch(numberFormat, /\.\.\.options/, 'undefined fields must not overwrite formatter defaults');
  assert.match(numberFormat, /minimumFractionDigits:\s*options\?\.minimumFractionDigits/);
  assert.match(numberFormat, /maximumFractionDigits:\s*options\?\.maximumFractionDigits/);
});

test('data storage, mobile quota, and system layout regressions stay on their safe paths', () => {
  assert.match(
    readFrontend('src/features/data-storages/index.tsx'),
    /const columns = useMemo\(\(\) => createColumns\(t, defaultDataStorageID \?\? undefined\), \[t, defaultDataStorageID\]\)/
  );

  const quota = readFrontend('src/components/quota-badges.tsx');
  assert.match(quota, /w-\[640px\] max-w-\[calc\(100vw-2rem\)\]/);
  assert.match(quota, /w-80 max-w-\[calc\(100vw-2rem\)\]/);

  assert.match(readFrontend('src/authenticated-layout.tsx'), /fixed inset-0 min-h-0 flex-col overflow-hidden/);
  assert.doesNotMatch(readFrontend('src/features/system/index.tsx'), /flex-1 overflow-auto/);
});

test('OTLP HTTP exporter spelling is valid in configuration and both deployment guides', () => {
  for (const path of [
    'config.example.yml',
    'docs/en/deployment/configuration.md',
    'docs/zh/deployment/configuration.md',
  ]) {
    const source = readFileSync(join(repositoryRoot, path), 'utf8');
    assert.match(source, /type:\s*['"]otlphttp['"]/);
    assert.doesNotMatch(source, /oltphttp/);
  }
});
