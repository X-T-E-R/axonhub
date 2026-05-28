import { z } from 'zod';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';
import { toast } from 'sonner';
import i18n from '@/lib/i18n';
import { useErrorHandler } from '@/hooks/use-error-handler';
import {
  channelKeyHealthCheckRuleSchema,
  channelKeyBalanceSnapshotSchema,
  channelKeyStatusSchema,
  failurePolicyProfileSchema,
  type ChannelKeyHealthCheckRule,
  type ChannelKeyStatus,
  type FailurePolicyProfile,
} from '@/features/channels/data/schema';

const monitoringNumber = (fallback: number, min = 0) =>
  z.preprocess((value) => (value === '' || value == null ? fallback : value), z.coerce.number().int().min(min).catch(fallback));

const monitoringArray = <T extends z.ZodType>(schema: T) => z.preprocess((value) => value ?? [], z.array(schema).catch([]));

function pickAlias(record: Record<string, unknown>, camel: string, snake: string, fallback: unknown) {
  return record[camel] ?? record[snake] ?? fallback;
}

function normalizeMonitoringScheduleInput(value: unknown): unknown {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return {};
  }

  const record = value as Record<string, unknown>;
  return {
    intervalMinutes: pickAlias(record, 'intervalMinutes', 'interval_minutes', 60),
    historyLimit: pickAlias(record, 'historyLimit', 'history_limit', 100),
    maxChannels: pickAlias(record, 'maxChannels', 'max_channels', 4),
    maxKeysPerChannel: pickAlias(record, 'maxKeysPerChannel', 'max_keys_per_channel', 8),
    keySpacingMs: pickAlias(record, 'keySpacingMs', 'key_spacing_ms', 1000),
    jitterMs: pickAlias(record, 'jitterMs', 'jitter_ms', 250),
  };
}

function normalizeMonitoringTargetsInput(value: unknown): unknown {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return {};
  }

  const record = value as Record<string, unknown>;
  return {
    channelIDs: pickAlias(record, 'channelIDs', 'channel_ids', []),
    channelStatuses: pickAlias(record, 'channelStatuses', 'channel_statuses', ['enabled']),
    keyStatuses: pickAlias(record, 'keyStatuses', 'key_statuses', ['active']),
    includeBackoff: pickAlias(record, 'includeBackoff', 'include_backoff', false),
  };
}

export const monitoringRuleScheduleSchema = z
  .preprocess(
    normalizeMonitoringScheduleInput,
    z.object({
      intervalMinutes: monitoringNumber(60, 1),
      historyLimit: monitoringNumber(100, 1),
      maxChannels: monitoringNumber(4, 0),
      maxKeysPerChannel: monitoringNumber(8, 0),
      keySpacingMs: monitoringNumber(1000, 0),
      jitterMs: monitoringNumber(250, 0),
    })
  )
  .default({
    intervalMinutes: 60,
    historyLimit: 100,
    maxChannels: 4,
    maxKeysPerChannel: 8,
    keySpacingMs: 1000,
    jitterMs: 250,
  });

export const monitoringRuleTargetsSchema = z
  .preprocess(
    normalizeMonitoringTargetsInput,
    z.object({
      channelIDs: z.preprocess((value) => value ?? [], z.array(z.number().int())),
      channelStatuses: z.preprocess((value) => value ?? ['enabled'], z.array(z.string())),
      keyStatuses: z.preprocess((value) => value ?? ['active'], z.array(channelKeyStatusSchema)),
      includeBackoff: z.preprocess((value) => value ?? false, z.boolean()),
    })
  )
  .default({
    channelIDs: [],
    channelStatuses: ['enabled'],
    keyStatuses: ['active'],
    includeBackoff: false,
  });

export const monitoringRuleSchema = z.object({
  id: z.string().min(1),
  name: z.string().min(1),
  description: z.string().optional().nullable(),
  enabled: z.preprocess((value) => value ?? true, z.boolean()),
  probeType: z.string().optional().nullable(),
  schedule: z.preprocess((value) => value ?? {}, monitoringRuleScheduleSchema),
  targets: z.preprocess((value) => value ?? {}, monitoringRuleTargetsSchema),
  probes: monitoringArray(channelKeyHealthCheckRuleSchema),
  keyProfiles: monitoringArray(failurePolicyProfileSchema),
  channelProfiles: monitoringArray(failurePolicyProfileSchema),
});

export const monitoringSettingsSchema = z.object({
  enabled: z.preprocess((value) => value ?? false, z.boolean()),
  historyRetentionDays: monitoringNumber(30, 1),
  rules: z.preprocess((value) => value ?? [], z.array(monitoringRuleSchema)),
});

export const monitoringEventSchema = z.object({
  id: z.string(),
  createdAt: z.string(),
  updatedAt: z.string(),
  channelID: z.string(),
  channelName: z.string().optional().nullable(),
  keyID: z.string().optional().nullable(),
  maskedKey: z.string().optional().nullable(),
  ruleID: z.string().optional().nullable(),
  ruleName: z.string().optional().nullable(),
  trigger: z.string(),
  source: z.string().optional().nullable(),
  success: z.boolean(),
  skipped: z.boolean(),
  reason: z.string().optional().nullable(),
  statusCode: z.number().int().optional().nullable(),
  balance: z.unknown().optional().nullable(),
  currency: z.string().optional().nullable(),
  available: z.boolean().optional().nullable(),
  balanceSnapshot: channelKeyBalanceSnapshotSchema.optional().nullable(),
  probe: z.string().optional().nullable(),
  matchedPolicy: z.string().optional().nullable(),
  action: z.string().optional().nullable(),
  nextCheckAt: z.string().optional().nullable(),
  backoffAttempt: z.number().int().optional().nullable(),
  checkedAt: z.string(),
});

export const monitoringEventConnectionSchema = z.object({
  edges: z.array(
    z.object({
      cursor: z.string(),
      node: monitoringEventSchema.nullable(),
    })
  ),
  pageInfo: z.object({
    hasNextPage: z.boolean(),
    hasPreviousPage: z.boolean(),
    startCursor: z.string().optional().nullable(),
    endCursor: z.string().optional().nullable(),
  }),
  totalCount: z.number(),
});

export type MonitoringRuleSchedule = z.infer<typeof monitoringRuleScheduleSchema>;
export type MonitoringRuleTargets = z.infer<typeof monitoringRuleTargetsSchema>;
export type MonitoringRule = z.infer<typeof monitoringRuleSchema>;
export type MonitoringSettings = z.infer<typeof monitoringSettingsSchema>;
export type MonitoringEvent = z.infer<typeof monitoringEventSchema>;
export type MonitoringEventConnection = z.infer<typeof monitoringEventConnectionSchema>;

export type UpdateMonitoringSettingsInput = {
  enabled?: boolean;
  historyRetentionDays?: number;
  rules?: MonitoringRuleInput[];
};

export type MonitoringRuleInput = {
  id: string;
  name: string;
  description?: string | null;
  enabled?: boolean | null;
  probeType?: string | null;
  schedule?: Partial<MonitoringRuleSchedule>;
  targets?: {
    channelIDs?: number[];
    channelStatuses?: string[];
    keyStatuses?: ChannelKeyStatus[];
    includeBackoff?: boolean;
  };
  probes?: ChannelKeyHealthCheckRule[];
  keyProfiles?: FailurePolicyProfile[];
  channelProfiles?: FailurePolicyProfile[];
};

export type MonitoringEventsVariables = {
  first?: number;
  after?: string;
  before?: string;
  last?: number;
  orderBy?: {
    field: 'CREATED_AT' | 'UPDATED_AT' | 'CHANNEL_ID' | 'KEY_ID' | 'RULE_ID' | 'TRIGGER' | 'SUCCESS' | 'SKIPPED' | 'CHECKED_AT';
    direction: 'ASC' | 'DESC';
  };
  where?: Record<string, unknown>;
};

const MONITORING_PROFILE_FIELDS = `
  id
  name
  enabled
  sources
  conditionCombiner
  conditions {
    minFailureCount
    success
    statusCodes
    available
    balanceLTE
    balanceGTE
    reasonContains
    allCheckedKeysFailed
    keyStatuses
    expr
  }
  actions {
    type
    backoff {
      mode
      intervalMinutes
      maxIntervalMinutes
      multiplier
    }
  }
`;

const MONITORING_SETTINGS_QUERY = `
  query MonitoringSettings {
    monitoringSettings {
      enabled
      historyRetentionDays
      rules {
        id
        name
        description
        enabled
        probeType
        schedule {
          intervalMinutes
          historyLimit
          maxChannels
          maxKeysPerChannel
          keySpacingMs
          jitterMs
        }
        targets {
          channelIDs
          channelStatuses
          keyStatuses
          includeBackoff
        }
        probes {
          id
          name
          type
          enabled
          builtin {
            kind
          }
          http {
            method
            urlMode
            path
            url
            timeoutMs
            headers {
              key
              value
            }
            keyInjection {
              location
              headerName
            }
            expectedStatuses
            passWhen
          }
        }
        keyProfiles {
          ${MONITORING_PROFILE_FIELDS}
        }
        channelProfiles {
          ${MONITORING_PROFILE_FIELDS}
        }
      }
    }
  }
`;

const UPDATE_MONITORING_SETTINGS_MUTATION = `
  mutation UpdateMonitoringSettings($input: UpdateMonitoringSettingsInput!) {
    updateMonitoringSettings(input: $input)
  }
`;

const MONITORING_EVENTS_QUERY = `
  query ChannelKeyMonitoringEvents(
    $first: Int
    $after: Cursor
    $before: Cursor
    $last: Int
    $orderBy: ChannelKeyMonitoringEventOrder
    $where: ChannelKeyMonitoringEventWhereInput
  ) {
    channelKeyMonitoringEvents(first: $first, after: $after, before: $before, last: $last, orderBy: $orderBy, where: $where) {
      edges {
        cursor
        node {
          id
          createdAt
          updatedAt
          channelID
          channelName
          keyID
          maskedKey
          ruleID
          ruleName
          trigger
          source
          success
          skipped
          reason
          statusCode
          balance
          currency
          available
          probe
          matchedPolicy
          action
          nextCheckAt
          backoffAttempt
          checkedAt
        }
      }
      pageInfo {
        hasNextPage
        hasPreviousPage
        startCursor
        endCursor
      }
      totalCount
    }
  }
`;

export function useMonitoringSettings() {
  const { handleError } = useErrorHandler();

  return useQuery({
    queryKey: ['monitoringSettings'],
    queryFn: async () => {
      try {
        const data = await graphqlRequest<{ monitoringSettings: unknown }>(MONITORING_SETTINGS_QUERY);
        return monitoringSettingsSchema.parse(data.monitoringSettings);
      } catch (error) {
        handleError(error, i18n.t('common.errors.internalServerError'));
        throw error;
      }
    },
  });
}

export function useUpdateMonitoringSettings() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (input: UpdateMonitoringSettingsInput) => {
      const data = await graphqlRequest<{ updateMonitoringSettings: boolean }>(UPDATE_MONITORING_SETTINGS_MUTATION, { input });
      return data.updateMonitoringSettings;
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['monitoringSettings'] });
      queryClient.invalidateQueries({ queryKey: ['monitoringEvents'] });
      toast.success(i18n.t('monitoring.messages.saveSuccess'));
    },
    onError: () => {
      toast.error(i18n.t('monitoring.messages.saveError'));
    },
  });
}

export function useMonitoringEvents(variables: MonitoringEventsVariables) {
  const { handleError } = useErrorHandler();

  return useQuery({
    queryKey: ['monitoringEvents', variables],
    queryFn: async () => {
      try {
        const data = await graphqlRequest<{ channelKeyMonitoringEvents: unknown }>(MONITORING_EVENTS_QUERY, variables);
        return monitoringEventConnectionSchema.parse(data.channelKeyMonitoringEvents);
      } catch (error) {
        handleError(error, i18n.t('common.errors.internalServerError'));
        throw error;
      }
    },
  });
}
