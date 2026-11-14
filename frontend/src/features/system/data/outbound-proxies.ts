import { z } from 'zod';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { toast } from 'sonner';
import { graphqlRequest } from '@/gql/graphql';
import i18n from '@/lib/i18n';
import { useErrorHandler } from '@/hooks/use-error-handler';

export const outboundProxyKindSchema = z.enum(['static', 'dynamic_template']);
export type OutboundProxyKind = z.infer<typeof outboundProxyKindSchema>;

export const outboundProxyStatusSchema = z.enum(['enabled', 'disabled']);
export type OutboundProxyStatus = z.infer<typeof outboundProxyStatusSchema>;

export const outboundProxySchema = z.object({
  id: z.string(),
  name: z.string(),
  kind: outboundProxyKindSchema,
  maskedUrl: z.string(),
  poolName: z.string(),
  status: outboundProxyStatusSchema,
  weight: z.number().int(),
  maxConcurrent: z.number().int().nullable().optional(),
  maxCredentials: z.number().int().nullable().optional(),
  backupProxyId: z.number().int().nullable().optional(),
  expiresAt: z.string().nullable().optional(),
  remark: z.string(),
  lastTestedAt: z.string().nullable().optional(),
  lastTestSuccess: z.boolean().nullable().optional(),
  lastExitIp: z.string().nullable().optional(),
  lastCountry: z.string().nullable().optional(),
  lastLatencyMs: z.number().int().nullable().optional(),
  lastTestError: z.string().nullable().optional(),
  credentialReferenceCount: z.number().int(),
  primaryCredentialCount: z.number().int(),
  backupCredentialCount: z.number().int(),
  backupProxyCount: z.number().int(),
  channelReferenceCount: z.number().int(),
  createdAt: z.string(),
  updatedAt: z.string(),
});
export type OutboundProxy = z.infer<typeof outboundProxySchema>;

export const outboundProxyPoolSchema = z.object({
  name: z.string(),
  proxyCount: z.number().int(),
  enabledProxyCount: z.number().int(),
  credentialReferenceCount: z.number().int(),
});
export type OutboundProxyPool = z.infer<typeof outboundProxyPoolSchema>;

export interface CreateOutboundProxyInput {
  name: string;
  kind?: OutboundProxyKind;
  proxyUrl: string;
  poolName?: string;
  status?: OutboundProxyStatus;
  weight?: number;
  maxConcurrent?: number;
  maxCredentials?: number;
  backupProxyId?: number;
  expiresAt?: string;
  remark?: string;
}

export interface UpdateOutboundProxyInput {
  name?: string;
  kind?: OutboundProxyKind;
  proxyUrl?: string;
  poolName?: string;
  status?: OutboundProxyStatus;
  weight?: number;
  maxConcurrent?: number;
  clearMaxConcurrent?: boolean;
  maxCredentials?: number;
  clearMaxCredentials?: boolean;
  backupProxyId?: number;
  clearBackupProxyId?: boolean;
  expiresAt?: string;
  clearExpiresAt?: boolean;
  remark?: string;
}

const OUTBOUND_PROXY_FIELDS = `
  id
  name
  kind
  maskedUrl
  poolName
  status
  weight
  maxConcurrent
  maxCredentials
  backupProxyId
  expiresAt
  remark
  lastTestedAt
  lastTestSuccess
  lastExitIp
  lastCountry
  lastLatencyMs
  lastTestError
  credentialReferenceCount
  primaryCredentialCount
  backupCredentialCount
  backupProxyCount
  channelReferenceCount
  createdAt
  updatedAt
`;

const OUTBOUND_PROXIES_QUERY = `
  query OutboundProxies {
    outboundProxies {
      ${OUTBOUND_PROXY_FIELDS}
    }
  }
`;

const OUTBOUND_PROXY_POOLS_QUERY = `
  query OutboundProxyPools {
    outboundProxyPools {
      name
      proxyCount
      enabledProxyCount
      credentialReferenceCount
    }
  }
`;

const CREATE_OUTBOUND_PROXY_MUTATION = `
  mutation CreateOutboundProxy($input: CreateOutboundProxyInput!) {
    createOutboundProxy(input: $input) {
      ${OUTBOUND_PROXY_FIELDS}
    }
  }
`;

const UPDATE_OUTBOUND_PROXY_MUTATION = `
  mutation UpdateOutboundProxy($id: ID!, $input: UpdateOutboundProxyInput!) {
    updateOutboundProxy(id: $id, input: $input) {
      ${OUTBOUND_PROXY_FIELDS}
    }
  }
`;

const DELETE_OUTBOUND_PROXY_MUTATION = `
  mutation DeleteOutboundProxy($id: ID!) {
    deleteOutboundProxy(id: $id)
  }
`;

const PROBE_OUTBOUND_PROXY_MUTATION = `
  mutation ProbeOutboundProxy($id: ID!) {
    probeOutboundProxy(id: $id) {
      ${OUTBOUND_PROXY_FIELDS}
    }
  }
`;

function invalidateOutboundProxyQueries(queryClient: ReturnType<typeof useQueryClient>) {
  queryClient.invalidateQueries({ queryKey: ['outboundProxies'] });
  queryClient.invalidateQueries({ queryKey: ['outboundProxyPools'] });
  queryClient.invalidateQueries({ queryKey: ['channelAPIKeyInventory'] });
}

export function useOutboundProxies(options?: { enabled?: boolean }) {
  const { handleError } = useErrorHandler();

  return useQuery({
    queryKey: ['outboundProxies'],
    enabled: options?.enabled ?? true,
    queryFn: async () => {
      try {
        const data = await graphqlRequest<{ outboundProxies: OutboundProxy[] }>(OUTBOUND_PROXIES_QUERY);
        return z.array(outboundProxySchema).parse(data.outboundProxies);
      } catch (error) {
        handleError(error, i18n.t('common.errors.internalServerError'));
        throw error;
      }
    },
  });
}

export function useOutboundProxyPools(options?: { enabled?: boolean }) {
  const { handleError } = useErrorHandler();

  return useQuery({
    queryKey: ['outboundProxyPools'],
    enabled: options?.enabled ?? true,
    queryFn: async () => {
      try {
        const data = await graphqlRequest<{ outboundProxyPools: OutboundProxyPool[] }>(OUTBOUND_PROXY_POOLS_QUERY);
        return z.array(outboundProxyPoolSchema).parse(data.outboundProxyPools);
      } catch (error) {
        handleError(error, i18n.t('common.errors.internalServerError'));
        throw error;
      }
    },
  });
}

export function useCreateOutboundProxy() {
  const queryClient = useQueryClient();
  const { handleError } = useErrorHandler();

  return useMutation({
    mutationFn: async (input: CreateOutboundProxyInput) => {
      const data = await graphqlRequest<{ createOutboundProxy: OutboundProxy }>(CREATE_OUTBOUND_PROXY_MUTATION, { input });
      return outboundProxySchema.parse(data.createOutboundProxy);
    },
    onSuccess: () => {
      invalidateOutboundProxyQueries(queryClient);
      toast.success(i18n.t('system.outboundProxies.messages.createSuccess'));
    },
    onError: (error) => {
      handleError(error, i18n.t('system.outboundProxies.messages.createFailed'));
    },
  });
}

export function useUpdateOutboundProxy() {
  const queryClient = useQueryClient();
  const { handleError } = useErrorHandler();

  return useMutation({
    mutationFn: async ({ id, input }: { id: string; input: UpdateOutboundProxyInput }) => {
      const data = await graphqlRequest<{ updateOutboundProxy: OutboundProxy }>(UPDATE_OUTBOUND_PROXY_MUTATION, { id, input });
      return outboundProxySchema.parse(data.updateOutboundProxy);
    },
    onSuccess: () => {
      invalidateOutboundProxyQueries(queryClient);
      toast.success(i18n.t('system.outboundProxies.messages.updateSuccess'));
    },
    onError: (error) => {
      handleError(error, i18n.t('system.outboundProxies.messages.updateFailed'));
    },
  });
}

export function useDeleteOutboundProxy() {
  const queryClient = useQueryClient();
  const { handleError } = useErrorHandler();

  return useMutation({
    mutationFn: async (id: string) => {
      const data = await graphqlRequest<{ deleteOutboundProxy: boolean }>(DELETE_OUTBOUND_PROXY_MUTATION, { id });
      return data.deleteOutboundProxy;
    },
    onSuccess: () => {
      invalidateOutboundProxyQueries(queryClient);
      toast.success(i18n.t('system.outboundProxies.messages.deleteSuccess'));
    },
    onError: (error) => {
      handleError(error, i18n.t('system.outboundProxies.messages.deleteFailed'));
    },
  });
}

export function useProbeOutboundProxy() {
  const queryClient = useQueryClient();
  const { handleError } = useErrorHandler();

  return useMutation({
    mutationFn: async ({ id }: { id: string; silent?: boolean }) => {
      const data = await graphqlRequest<{ probeOutboundProxy: OutboundProxy }>(PROBE_OUTBOUND_PROXY_MUTATION, { id });
      return outboundProxySchema.parse(data.probeOutboundProxy);
    },
    onSuccess: (proxy, variables) => {
      invalidateOutboundProxyQueries(queryClient);
      if (variables.silent) {
        return;
      }
      if (proxy.lastTestSuccess) {
        toast.success(
          i18n.t('system.outboundProxies.messages.probeSuccess', {
            name: proxy.name,
            ip: proxy.lastExitIp || '-',
            latency: proxy.lastLatencyMs ?? 0,
          })
        );
      } else {
        toast.error(proxy.lastTestError || i18n.t('system.outboundProxies.messages.probeFailed'));
      }
    },
    onError: (error, variables) => {
      if (!variables.silent) {
        handleError(error, i18n.t('system.outboundProxies.messages.probeFailed'));
      }
    },
  });
}
