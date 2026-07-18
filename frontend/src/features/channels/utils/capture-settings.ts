import type { ChannelSettings } from '../data/schema';

export type AxonHubCaptureSettings = {
  storeExecutionRequestBody: boolean | null;
  storeExecutionResponseBody: boolean | null;
  storeExecutionStreamChunks: boolean | null;
};

export type AxonHubCapturePreset = 'inherit' | 'fullDebug' | 'lowLatency';
export type AxonHubCapturePresetValue = AxonHubCapturePreset | 'custom';

export const AXONHUB_CAPTURE_PRESETS: Record<AxonHubCapturePreset, AxonHubCaptureSettings> = {
  inherit: {
    storeExecutionRequestBody: null,
    storeExecutionResponseBody: null,
    storeExecutionStreamChunks: null,
  },
  fullDebug: {
    storeExecutionRequestBody: true,
    storeExecutionResponseBody: true,
    storeExecutionStreamChunks: true,
  },
  lowLatency: {
    storeExecutionRequestBody: false,
    storeExecutionResponseBody: false,
    storeExecutionStreamChunks: false,
  },
};

export function resolveAxonHubCapturePreset(value: string): AxonHubCaptureSettings | null {
  if (!Object.prototype.hasOwnProperty.call(AXONHUB_CAPTURE_PRESETS, value)) return null;
  return { ...AXONHUB_CAPTURE_PRESETS[value as AxonHubCapturePreset] };
}

export function getAxonHubCaptureSettings(settings?: ChannelSettings | null): AxonHubCaptureSettings {
  return {
    storeExecutionRequestBody: settings?.storeExecutionRequestBody ?? null,
    storeExecutionResponseBody: settings?.storeExecutionResponseBody ?? null,
    storeExecutionStreamChunks: settings?.storeExecutionStreamChunks ?? null,
  };
}

export function getAxonHubCapturePreset(settings: AxonHubCaptureSettings): AxonHubCapturePresetValue {
  for (const preset of Object.keys(AXONHUB_CAPTURE_PRESETS) as AxonHubCapturePreset[]) {
    const candidate = AXONHUB_CAPTURE_PRESETS[preset];
    if (
      candidate.storeExecutionRequestBody === settings.storeExecutionRequestBody &&
      candidate.storeExecutionResponseBody === settings.storeExecutionResponseBody &&
      candidate.storeExecutionStreamChunks === settings.storeExecutionStreamChunks
    ) {
      return preset;
    }
  }

  return 'custom';
}
