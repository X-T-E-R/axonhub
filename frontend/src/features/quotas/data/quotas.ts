import { z } from 'zod';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { graphqlRequest } from '@/gql/graphql';
import { useErrorHandler } from '@/hooks/use-error-handler';
import { CHANNEL_QUOTA_OVERVIEW_FIELDS } from '@/features/channels/data/channels';
import {
  channelQuotaOverviewSchema,
  channelStatusSchema,
  channelTypeSchema,
} from '@/features/channels/data/schema';

export const quotaChannelItemSchema = z.object({
  id: z.string(),
  name: z.string(),
  type: channelTypeSchema,
  status: channelStatusSchema,
  baseURL: z.string(),
  settings: z
    .object({
      rawHTTP: z
        .object({
          enabled: z.boolean(),
          resource: z.string(),
        })
        .optional()
        .nullable(),
      balanceProbe: z
        .object({
          enabled: z.boolean().optional().nullable(),
          preset: z.string().optional().nullable(),
          sharedWallet: z.boolean().optional().nullable(),
          hasProbeAPIKey: z.boolean().optional().nullable(),
        })
        .optional()
        .nullable(),
    })
    .optional()
    .nullable(),
  quotaOverview: channelQuotaOverviewSchema.optional().nullable(),
});

export type QuotaChannelItem = z.infer<typeof quotaChannelItemSchema>;

const QUERY_QUOTA_CHANNELS_QUERY = `
  query QueryQuotaChannels($input: QueryChannelInput!) {
    queryChannels(input: $input) {
      edges {
        node {
          id
          name
          type
          status
          baseURL
          settings {
            rawHTTP {
              enabled
              resource
            }
            balanceProbe {
              enabled
              preset
              sharedWallet
              hasProbeAPIKey
            }
          }
          quotaOverview {
            ${CHANNEL_QUOTA_OVERVIEW_FIELDS}
          }
        }
      }
      totalCount
    }
  }
`;

export function useQuotaChannels(options?: { search?: string; includeDisabled?: boolean }) {
  const { handleError } = useErrorHandler();
  const { t } = useTranslation();
  const search = options?.search?.trim() || undefined;
  const includeDisabled = options?.includeDisabled ?? true;

  return useQuery({
    queryKey: ['quotas-channels', search ?? '', includeDisabled],
    queryFn: async () => {
      try {
        const where: Record<string, unknown> = {
          statusIn: includeDisabled ? ['enabled', 'disabled'] : ['enabled'],
        };
        if (search) {
          where.nameContainsFold = search;
        }
        const data = await graphqlRequest<{
          queryChannels: {
            edges: Array<{ node: QuotaChannelItem }>;
            totalCount: number;
          };
        }>(QUERY_QUOTA_CHANNELS_QUERY, {
          input: {
            first: 200,
            where,
            orderBy: { field: 'ORDERING_WEIGHT', direction: 'DESC' },
          },
        });
        const items = (data?.queryChannels?.edges ?? []).map((edge) => quotaChannelItemSchema.parse(edge.node));
        return {
          items,
          totalCount: data?.queryChannels?.totalCount ?? items.length,
        };
      } catch (error) {
        handleError(error, t('common.errors.internalServerError'));
        throw error;
      }
    },
    staleTime: 30_000,
  });
}
