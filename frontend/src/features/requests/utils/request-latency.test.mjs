import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

import { formatLatencyMs, resolveExecutionLatencyMs } from './request-latency.ts';

test('execution latency prefers persisted metrics over delayed record updates', () => {
  const latency = resolveExecutionLatencyMs(6638, '2026-08-30T15:50:28.504Z', '2026-08-30T15:59:35.120Z');
  assert.equal(latency, 6638);
  assert.equal(formatLatencyMs(latency, 'Unknown'), '6.64s');
});

test('execution latency falls back to valid timestamps only when metrics are unavailable', () => {
  assert.equal(resolveExecutionLatencyMs(null, '2026-08-30T15:50:28.504Z', '2026-08-30T15:59:35.120Z'), 546616);
  assert.equal(resolveExecutionLatencyMs(Number.NaN, '2026-08-30T15:50:28.504Z', '2026-08-30T15:50:29.504Z'), 1000);
  assert.equal(resolveExecutionLatencyMs(undefined, 'invalid', '2026-08-30T15:50:29.504Z'), null);
  assert.equal(resolveExecutionLatencyMs(undefined, '2026-08-30T15:50:30.504Z', '2026-08-30T15:50:29.504Z'), null);
});

test('request execution details fetch and use persisted latency metrics', () => {
  const featureRoot = join(import.meta.dirname, '..');
  const query = readFileSync(join(featureRoot, 'data', 'requests.ts'), 'utf8');
  const detail = readFileSync(join(featureRoot, 'components', 'request-detail-content.tsx'), 'utf8');

  const executionQuery = query.slice(query.indexOf('function buildRequestExecutionsQuery'), query.indexOf('// Query hooks'));
  assert.match(executionQuery, /passThroughApplied\s+metricsLatencyMs\s+metricsFirstTokenLatencyMs/);
  assert.match(detail, /resolveExecutionLatencyMs\(\s*execution\.metricsLatencyMs,/);
  assert.doesNotMatch(detail, /calculateLatency\(execution\.createdAt/);
});
