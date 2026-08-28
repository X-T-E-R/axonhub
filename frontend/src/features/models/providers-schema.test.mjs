import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { providerModelSchema, providersDataSchema } from './data/providers.schema.ts';

test('provider model schema accepts boolean and object experimental metadata', () => {
  assert.equal(providerModelSchema.parse({ id: 'boolean-experimental', experimental: true }).experimental, true);
  assert.deepEqual(
    providerModelSchema.parse({ id: 'object-experimental', experimental: { modes: { fast: { provider: { tier: 'fast' } } } } })
      .experimental,
    { modes: { fast: { provider: { tier: 'fast' } } } }
  );
});

test('the selectively refreshed provider catalog satisfies the local schema', async () => {
  const catalog = JSON.parse(await readFile(new URL('./data/providers.json', import.meta.url), 'utf8'));
  const parsed = providersDataSchema.parse(catalog);

  assert.ok(parsed.providers.deepseek.models.some((model) => model.id === 'deepseek-v4-flash-vision-exp'));
  assert.ok(parsed.providers.xai.models.some((model) => model.id === 'grok-imagine-image-2.0'));
});
