import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';

const read = (path) => fs.readFileSync(new URL(path, import.meta.url), 'utf8');

test('AxonHub provider exposes the moderation API format', () => {
  const providers = read('./channels/data/config_providers.ts');
  const start = providers.indexOf('const AXONHUB_PROVIDER_API_FORMATS');
  const end = providers.indexOf('];', start);
  assert.ok(start >= 0 && end > start, 'AxonHub provider format list must exist');
  assert.match(providers.slice(start, end), /'openai\/moderations'/);
});

test('moderation results render in the response preview', () => {
  const parser = read('./requests/utils/response-parser.ts');
  assert.match(parser, /Array\.isArray\(body\.results\)/);
  assert.match(parser, /results:\s*body\.results/);
});
