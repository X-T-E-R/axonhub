import { isNativeDecisionFormat } from '@/features/channels/data/native-decisions';

export type BodyEvidence = {
  format?: string;
  requestBody?: unknown;
  responseBody?: unknown;
  requestBodyRaw?: string | null;
  responseBodyRaw?: string | null;
};
export type BodyKind = 'requestBody' | 'responseBody';

// Native JSON must never be rebuilt from the legacy JSON scalar's JS Numbers.
// A missing raw field is unavailable evidence, not permission to use rounded data.
export function bodyEvidence(record: BodyEvidence, kind: BodyKind): unknown {
  return isNativeDecisionFormat(record.format) ? (record[`${kind}Raw`] ?? '') : record[kind];
}

export function formatBodyEvidence(record: BodyEvidence, kind: BodyKind): string {
  const body = bodyEvidence(record, kind);
  if (isNativeDecisionFormat(record.format)) return body as string;
  if (!body) return '';
  return JSON.stringify(body, null, 2);
}
