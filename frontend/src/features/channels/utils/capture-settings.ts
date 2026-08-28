import type { ChannelSettings } from '../data/schema';

export type AxonHubCaptureSettings = {
  storeExecutionRequestBody: boolean | null;
  storeExecutionResponseBody: boolean | null;
  storeExecutionStreamChunks: boolean | null;
};

export type AxonHubCapturePreset = 'inherit' | 'fullDebug' | 'lowLatency';
export type AxonHubCapturePresetValue = AxonHubCapturePreset | 'custom';

export type AxonHubBehaviorSettings = AxonHubCaptureSettings & {
  passThroughUserAgent: boolean | null;
  passThroughBody: boolean | null;
  disableRetries: boolean;
  fullPassThrough: boolean;
};

export type AxonHubBehaviorPreset = 'lowLatency' | 'standard' | 'audit' | 'fullDebug';
export type AxonHubBehaviorPresetValue = AxonHubBehaviorPreset | 'custom';

export type GenericChannelBehaviorSettings = Pick<
  AxonHubBehaviorSettings,
  'passThroughUserAgent' | 'passThroughBody' | 'disableRetries'
>;

export type AxonHubScopedSettingsTransition = {
  settings: GenericChannelBehaviorSettings;
  restoreSettings: GenericChannelBehaviorSettings | null;
  enteredAxonHub: boolean;
  leftAxonHub: boolean;
};

export const DEFAULT_GENERIC_CHANNEL_BEHAVIOR_SETTINGS: GenericChannelBehaviorSettings = {
  passThroughUserAgent: null,
  passThroughBody: null,
  disableRetries: false,
};

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

export const AXONHUB_BEHAVIOR_PRESETS: Record<AxonHubBehaviorPreset, AxonHubBehaviorSettings> = {
  lowLatency: {
    passThroughUserAgent: false,
    passThroughBody: true,
    disableRetries: true,
    fullPassThrough: true,
    storeExecutionRequestBody: false,
    storeExecutionResponseBody: false,
    storeExecutionStreamChunks: false,
  },
  standard: {
    passThroughUserAgent: null,
    passThroughBody: true,
    disableRetries: false,
    fullPassThrough: true,
    storeExecutionRequestBody: null,
    storeExecutionResponseBody: null,
    storeExecutionStreamChunks: null,
  },
  audit: {
    passThroughUserAgent: null,
    passThroughBody: false,
    disableRetries: false,
    fullPassThrough: true,
    storeExecutionRequestBody: true,
    storeExecutionResponseBody: true,
    storeExecutionStreamChunks: false,
  },
  fullDebug: {
    passThroughUserAgent: true,
    passThroughBody: true,
    disableRetries: true,
    fullPassThrough: true,
    storeExecutionRequestBody: true,
    storeExecutionResponseBody: true,
    storeExecutionStreamChunks: true,
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

export function getGenericChannelBehaviorSettings(settings?: ChannelSettings | null): GenericChannelBehaviorSettings {
  return {
    passThroughUserAgent: settings?.passThroughUserAgent ?? null,
    passThroughBody: settings?.passThroughBody ?? null,
    disableRetries: settings?.disableRetries ?? false,
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

export function resolveAxonHubBehaviorPreset(value: string): AxonHubBehaviorSettings | null {
  if (!Object.prototype.hasOwnProperty.call(AXONHUB_BEHAVIOR_PRESETS, value)) return null;
  return { ...AXONHUB_BEHAVIOR_PRESETS[value as AxonHubBehaviorPreset] };
}

export function getAxonHubBehaviorPreset(settings: AxonHubBehaviorSettings): AxonHubBehaviorPresetValue {
  for (const preset of Object.keys(AXONHUB_BEHAVIOR_PRESETS) as AxonHubBehaviorPreset[]) {
    const candidate = AXONHUB_BEHAVIOR_PRESETS[preset];
    if (
      candidate.passThroughUserAgent === settings.passThroughUserAgent &&
      candidate.passThroughBody === settings.passThroughBody &&
      candidate.disableRetries === settings.disableRetries &&
      candidate.fullPassThrough === settings.fullPassThrough &&
      candidate.storeExecutionRequestBody === settings.storeExecutionRequestBody &&
      candidate.storeExecutionResponseBody === settings.storeExecutionResponseBody &&
      candidate.storeExecutionStreamChunks === settings.storeExecutionStreamChunks
    ) {
      return preset;
    }
  }

  return 'custom';
}

export function transitionAxonHubScopedSettings(
  currentSettings: GenericChannelBehaviorSettings,
  restoreSettings: GenericChannelBehaviorSettings | null,
  wasAxonHub: boolean,
  willBeAxonHub: boolean
): AxonHubScopedSettingsTransition {
  if (!wasAxonHub && willBeAxonHub) {
    return {
      settings: { ...currentSettings },
      restoreSettings: { ...currentSettings },
      enteredAxonHub: true,
      leftAxonHub: false,
    };
  }

  if (wasAxonHub && !willBeAxonHub) {
    return {
      settings: { ...(restoreSettings ?? DEFAULT_GENERIC_CHANNEL_BEHAVIOR_SETTINGS) },
      restoreSettings: null,
      enteredAxonHub: false,
      leftAxonHub: true,
    };
  }

  return {
    settings: { ...currentSettings },
    restoreSettings,
    enteredAxonHub: false,
    leftAxonHub: false,
  };
}

export function resolveGenericChannelBehaviorSettings(
  isAxonHub: boolean,
  currentSettings: GenericChannelBehaviorSettings,
  restoreSettings: GenericChannelBehaviorSettings | null
): GenericChannelBehaviorSettings {
  if (!isAxonHub && restoreSettings) {
    return { ...restoreSettings };
  }
  return { ...currentSettings };
}
