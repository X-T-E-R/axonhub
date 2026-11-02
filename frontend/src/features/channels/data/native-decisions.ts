import type { ChannelEndpoint } from './schema';

export const NATIVE_DECISION_PRESETS = {
  typesafe: {
    name: 'TypeSafe Jev',
    baseURL: 'https://api.typesafe.ai',
    models: ['jev-1.14.0', 'jev-latest', 'jev-preview'],
    apiFormat: 'typesafe/systemone',
    path: '/v1/systemone',
  },
  commandcode: {
    name: 'Command Code Jev',
    baseURL: 'https://api.commandcode.ai/provider',
    models: ['typesafe/jev-1.14.0', 'typesafe/jev'],
    apiFormat: 'commandcode/systemone',
    path: '/v1/systemone',
  },
  openai_decisions: {
    name: 'OpenAI Decisions',
    baseURL: 'https://api.openai.com/v1',
    models: ['gpt-6-luna'],
    apiFormat: 'openai/decisions',
    path: '/decisions',
  },
  systemone_compatible: { name: 'System One compatible', baseURL: '', models: [], apiFormat: 'typesafe/systemone', path: '/v1/systemone' },
} as const;

export type NativeDecisionPreset = keyof typeof NATIVE_DECISION_PRESETS;
export function isNativeDecisionPreset(value: string): value is NativeDecisionPreset {
  return Object.hasOwn(NATIVE_DECISION_PRESETS, value);
}
export function isNativeDecisionFormat(value?: string): boolean {
  return value === 'typesafe/systemone' || value === 'commandcode/systemone' || value === 'openai/decisions';
}

// Reopen from the saved endpoint, not the OpenAI carrier's chat defaults.
export function getNativeDecisionPreset(channel?: {
  type?: string;
  baseURL?: string;
  endpoints?: ChannelEndpoint[] | null;
}): NativeDecisionPreset | undefined {
  if (channel?.type !== 'openai' || !channel.endpoints?.length || !channel.endpoints.every((ep) => isNativeDecisionFormat(ep.apiFormat)))
    return undefined;
  const format = channel.endpoints[0].apiFormat;
  if (format === 'commandcode/systemone') return 'commandcode';
  if (format === 'openai/decisions') return 'openai_decisions';
  return channel.baseURL?.replace(/\/+$/, '') === NATIVE_DECISION_PRESETS.typesafe.baseURL ? 'typesafe' : 'systemone_compatible';
}

// This is the shared create/update endpoint payload. Custom endpoint overrides
// survive reopening; choosing another preset explicitly replaces the endpoint.
export function nativeDecisionEndpoints(preset: NativeDecisionPreset, saved?: ChannelEndpoint[] | null): ChannelEndpoint[] {
  const config = NATIVE_DECISION_PRESETS[preset];
  if (saved?.length && saved[0].apiFormat === config.apiFormat && saved.every((ep) => isNativeDecisionFormat(ep.apiFormat)))
    return saved.map((ep) => ({ ...ep }));
  return [{ apiFormat: config.apiFormat, path: config.path }];
}
