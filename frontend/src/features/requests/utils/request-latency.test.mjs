import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { formatLatencyMs, resolveExecutionLatencyMs } from './request-latency.ts';

test('execution latency prefers finite nonnegative persisted metrics', () => {
  assert.equal(resolveExecutionLatencyMs(6638, '2026-08-30T15:50:28.504Z', '2026-08-30T15:59:35.120Z'), 6638);
  assert.equal(resolveExecutionLatencyMs(0, '2026-08-30T15:50:28.504Z', '2026-08-30T15:59:35.120Z'), 0);
  assert.equal(formatLatencyMs(6638, 'Unknown'), '6.64s');
  assert.equal(formatLatencyMs(0, 'Unknown'), '0ms');
});

test('execution latency falls back to valid timestamps when metrics are absent or invalid', () => {
  assert.equal(resolveExecutionLatencyMs(null, '2026-08-30T15:50:28.504Z', '2026-08-30T15:59:35.120Z'), 546616);
  assert.equal(resolveExecutionLatencyMs(Number.NaN, '2026-08-30T15:50:28.504Z', '2026-08-30T15:50:29.504Z'), 1000);
  assert.equal(resolveExecutionLatencyMs(Number.POSITIVE_INFINITY, '2026-08-30T15:50:28.504Z', '2026-08-30T15:50:29.504Z'), 1000);
  assert.equal(resolveExecutionLatencyMs(-1, '2026-08-30T15:50:28.504Z', '2026-08-30T15:50:29.504Z'), 1000);
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
