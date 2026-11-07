import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { z } from 'zod';
import { apiRequest } from '@/lib/api-client';

const endpoint = '/admin/cluster/placement';

const observationSchema = z.object({
  placement_version: z.number(),
  owner: z.string(),
  change: z.object({
    request_id: z.string(),
    placement_version: z.number(),
    channel_ids: z.array(z.number()),
    from: z.string(),
    to: z.string(),
    saved_at: z.string(),
  }).optional(),
  target: z.object({ node: z.string(), placement_version: z.number(), ready: z.boolean() }).optional(),
  target_error: z.string().optional(),
  worker_ack: z.object({ placement_version: z.number(), worker_revision: z.string() }).optional(),
});

const previewSchema = z.object({
  placement_version: z.number(),
  channel_id: z.number(),
  from: z.string(),
  to: z.string(),
  channels: z.array(z.object({ id: z.number(), name: z.string(), status: z.enum(['enabled', 'disabled', 'archived']) })),
});

export type PlacementObservation = z.infer<typeof observationSchema>;

export function placementSyncState(value?: PlacementObservation) {
  const version = value?.change?.placement_version;
  return {
    targetReady: version !== undefined && !!value?.target?.ready && value.target.placement_version >= version,
    workerReady: version !== undefined && !!value?.worker_ack && value.worker_ack.placement_version >= version,
  };
}

export function usePlacementObservation(channelID: number, enabled: boolean) {
  return useQuery({
    queryKey: ['channelPlacement', channelID],
    enabled,
    queryFn: async () => observationSchema.parse(await apiRequest<unknown>(`${endpoint}/${channelID}`, { requireAuth: true })),
    refetchInterval: (query) => {
      const data = query.state.data;
      if (!data?.change) return false;
      const { targetReady, workerReady } = placementSyncState(data);
      return targetReady && workerReady ? false : 4000;
    },
  });
}

export function usePlacementPreview(channelID: number, target: string, enabled: boolean) {
  return useQuery({
    queryKey: ['channelPlacementPreview', channelID, target],
    enabled: enabled && !!target,
    queryFn: async () => previewSchema.parse(await apiRequest<unknown>(`${endpoint}/${channelID}/preview?${new URLSearchParams({ target })}`, {
      requireAuth: true,
    })),
  });
}

export function useSavePlacement(channelID: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: { target: string; expected_version: number; request_id: string; channel_ids: number[] }) => {
      const value = await apiRequest<unknown>(endpoint, { method: 'POST', body: { channel_id: channelID, ...input }, requireAuth: true });
      return observationSchema.parse(value);
    },
    onSuccess: (value) => {
      queryClient.setQueryData(['channelPlacement', channelID], value);
      void queryClient.invalidateQueries({ queryKey: ['channels'] });
    },
    onError: () => {
      void queryClient.invalidateQueries({ queryKey: ['channelPlacement', channelID] });
      void queryClient.invalidateQueries({ queryKey: ['channelPlacementPreview', channelID] });
    },
  });
}
