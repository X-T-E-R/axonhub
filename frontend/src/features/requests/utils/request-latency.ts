type TimestampValue = string | Date | null | undefined;

export function resolveExecutionLatencyMs(
  metricsLatencyMs: number | null | undefined,
  createdAt: TimestampValue,
  updatedAt: TimestampValue
): number | null {
  if (typeof metricsLatencyMs === 'number' && Number.isFinite(metricsLatencyMs) && metricsLatencyMs >= 0) {
    return metricsLatencyMs;
  }
  if (!createdAt || !updatedAt) return null;

  const start = new Date(createdAt).getTime();
  const end = new Date(updatedAt).getTime();
  if (!Number.isFinite(start) || !Number.isFinite(end) || end < start) return null;

  return end - start;
}

export function formatLatencyMs(latencyMs: number | null, unknownLabel: string): string {
  if (latencyMs === null) return unknownLabel;
  if (latencyMs < 1000) return `${latencyMs}ms`;
  return `${(latencyMs / 1000).toFixed(2)}s`;
}
