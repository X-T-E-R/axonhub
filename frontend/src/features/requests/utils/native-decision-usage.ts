import { isNativeDecisionFormat } from '@/features/channels/data/native-decisions';

// Ledger rows require both counters. Inspect the native response separately so
// absent/partial usage, cache details and pricing are never presented as zero.
export function nativeDecisionUsage(format?: string, responseBody?: unknown) {
  if (!isNativeDecisionFormat(format)) return undefined;
  const body = responseBody as
    | { usage?: { input_tokens?: number; output_tokens?: number; input_tokens_details?: { cached_tokens?: number } } }
    | undefined;
  const usage = body?.usage;
  return {
    complete: typeof usage?.input_tokens === 'number' && typeof usage?.output_tokens === 'number',
    cachedTokens: usage?.input_tokens_details?.cached_tokens ?? null,
  };
}
