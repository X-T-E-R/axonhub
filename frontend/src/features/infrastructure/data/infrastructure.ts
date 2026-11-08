import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiRequest } from '@/lib/api-client';

const BASE = '/admin/cluster/infrastructure';

export interface KomodoDiskSample {
  mount: string;
  usedGb: number;
  totalGb: number;
}

export interface KomodoMetrics {
  at: string;
  cpuPercent: number;
  memoryUsedGb: number;
  memoryTotalGb: number;
  disks: KomodoDiskSample[];
  networkIngressBytes?: number;
  networkEgressBytes?: number;
}

export interface KomodoObservation {
  nodeId: string;
  komodoServerId: string | null;
  registered: boolean;
  state: 'Ok' | 'NotOk' | 'Disabled' | 'unregistered' | 'unknown' | string;
  observedAt: string | null;
  statsRefreshTs: number | null;
  statsAt: string | null;
  ageSeconds?: number;
  metricsStatus: 'fresh' | 'pending' | 'stale' | 'unavailable' | string;
  metricsReason: string;
  metrics: KomodoMetrics | null;
  publicIp?: string | null;
  sshAlias?: string | null;
  aliases?: string[];
}

export interface InfraNode {
  id: string;
  name: string;
  provider: string;
  region?: string;
  roles: string[];
  ssh?: {
    host?: string;
    port?: number;
    configRef?: string;
  };
  vpn?: {
    proxyServer?: string;
    publicIp?: string;
    wireguardAddress?: string;
    manifestRef?: string;
    realityServerName?: string;
    realityTarget?: string;
    wireguardPeerNode?: string;
  };
  monitor?: {
    coreUrl?: string;
    server?: string | null;
    keyRef?: string;
    secretRef?: string;
    onboardingRef?: string;
    registrationRef?: string;
  };
  api?: {
    origin?: string;
    loopbackPort?: number;
    authMode?: 'axon-shared' | 'independent' | string;
    source?: 'existing' | 'image' | string;
    image?: string;
    configRef?: string;
    probeRef?: string;
  };
}

export interface InfraRuleMatch {
  method: 'POST';
  pathPrefix?: string;
  model?: string;
}

export interface InfraRule {
  id: string;
  name: string;
  enabled: boolean;
  priority: number;
  match: InfraRuleMatch;
  targetNode: string;
}

export interface InfraDiffEntry {
  section: 'nodes' | 'rules' | string;
  id: string;
  change: 'added' | 'removed' | 'changed' | string;
  before?: unknown;
  after?: unknown;
}

export interface InfraValidationIssue {
  path: string;
  code: string;
  message: string;
}

export interface InfraVersion {
  id: string;
  baseVersion?: string | null;
  revision: number;
  createdAt: string;
  activatedAt?: string | null;
  note?: string;
  status: 'active' | 'candidate' | 'superseded' | 'failed' | string;
  diff?: InfraDiffEntry[];
  readBack?: {
    adapter?: string;
    version?: string;
    at?: string;
  } | null;
}

export interface NodeProgressStep {
  status: string;
  at?: string | null;
  message?: string;
}

export interface NodeProgress {
  mode?: string;
  status?: string;
  steps?: Record<string, NodeProgressStep>;
  metrics?: KomodoMetrics | null;
  komodo?: KomodoObservation;
}

export interface InfrastructureStateResponse {
  revision: number;
  draft: {
    nodes: InfraNode[];
    rules: InfraRule[];
  };
  progress: Record<string, NodeProgress>;
  baseline?: {
    version?: string;
    control?: string;
    responsesMode?: string;
    defaults?: Record<string, string>;
    privateLane?: {
      origin?: string;
      secretRef?: string;
    } | null;
    inventory?: {
      sub2Runtime?: {
        activeNodeId?: string;
        standbyNodeId?: string;
        activeContainer?: string;
        activeImage?: string;
        workerVersion?: string;
        syncStatus?: string;
        originRoutes?: Record<string, string>;
      };
    };
  };
  activeVersion: string;
  versions: InfraVersion[];
  adapter?: string;
  edgeUrl?: string;
  liveSources: {
    nodeMap: {
      path: string;
      coreOrigin: string;
      generatedAt: string | null;
      nodeCount: number;
      nodes: Array<{
        nodeId: string;
        name: string;
        provider: string;
        publicIp: string | null;
        sshAlias: string | null;
        komodoServerId: string | null;
        aliases: string[];
      }>;
    };
    komodo: {
      coreOrigin: string;
      version: string;
      observedAt: string | null;
      lastSuccessAt: string | null;
      stale: boolean;
      error: string | null;
      nodes: Record<string, KomodoObservation>;
    };
    worker: {
      version: string | null;
      deployment: string | null;
      observedAt: string | null;
      source: string | null;
      defaults: Record<string, string> | null;
      control: string;
      responsesMode: string;
      privateLane: { origin?: string; secretRef?: string } | null;
      fixedModelOwners: Record<string, string>;
      historicalBaselineVersion: string;
    };
    axon: {
      clusterEnabled: boolean;
      controlNode: string;
      placementVersion: number;
      manifestGeneratedAt: string | null;
      modelCount: number;
      readyModelCount: number;
      registeredMachines: string[];
      workerAck: { placement_version: number; worker_revision: string; acknowledged_at?: string } | null;
      syncStatus: 'synchronized' | 'ack_pending' | 'static_manifest' | string;
    };
  };
}

export interface TopologyNodeItem {
  id: string;
  kind: 'entry' | 'worker' | 'node' | 'service' | 'egress' | string;
  label: string;
  address?: string | null;
  nodeId?: string | null;
  roles?: string[];
  status?: string;
  refs?: string[];
}

export interface TopologyConnectionItem {
  id: string;
  from: string;
  to: string;
  kind: string;
  status: string;
  label?: string;
  evidence?: string;
  observedAt?: string;
}

export interface TopologyGraphResponse {
  source: 'active' | 'draft' | 'production' | string;
  version: string;
  revision: number;
  adapter?: string;
  basis?: string;
  provenance?: Record<string, unknown>;
  nodes: TopologyNodeItem[];
  connections: TopologyConnectionItem[];
  observedProgress?: Record<string, NodeProgress>;
}

export interface ExplainRouteResponse {
  source: string;
  workerVersion?: string;
  owner: string | null;
  reason: string;
  ruleId: string | null;
  kind: string;
  conditional?: boolean;
  trace: Array<{
    code: string;
    message: string;
    ruleId?: string;
  }>;
}

export interface ReleasePreviewResponse {
  revision: number;
  baseVersion: string;
  valid: boolean;
  issues: InfraValidationIssue[];
  diff: InfraDiffEntry[];
}

export function useInfrastructureState(refresh = false) {
  return useQuery({
    queryKey: ['infrastructure', 'state', refresh],
    queryFn: () =>
      apiRequest<InfrastructureStateResponse>(
        `${BASE}/state${refresh ? '?refresh=1' : ''}`,
        { requireAuth: true }
      ),
  });
}

export function useInfrastructureTopology(source: 'active' | 'draft' | 'production' = 'active') {
  return useQuery({
    queryKey: ['infrastructure', 'topology', source],
    queryFn: () =>
      apiRequest<TopologyGraphResponse>(
        `${BASE}/topology?source=${encodeURIComponent(source)}`,
        { requireAuth: true }
      ),
  });
}

export function useRefreshInfrastructure() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (source: 'active' | 'draft' | 'production' = 'active') => {
      const [state, topology] = await Promise.all([
        apiRequest<InfrastructureStateResponse>(`${BASE}/state?refresh=1`, { requireAuth: true }),
        apiRequest<TopologyGraphResponse>(
          `${BASE}/topology?source=${encodeURIComponent(source)}&refresh=1`,
          { requireAuth: true }
        ),
      ]);
      return { state, topology, source };
    },
    onSuccess: ({ state, topology, source }) => {
      queryClient.setQueryData(['infrastructure', 'state', false], state);
      queryClient.setQueryData(['infrastructure', 'topology', source], topology);
      void queryClient.invalidateQueries({ queryKey: ['infrastructure'] });
    },
  });
}

export function useUpdateInfraNode() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({
      nodeId,
      revision,
      node,
    }: {
      nodeId: string;
      revision: number;
      node: InfraNode;
    }) =>
      apiRequest<InfrastructureStateResponse>(`${BASE}/nodes/${encodeURIComponent(nodeId)}`, {
        method: 'PUT',
        body: { revision, node },
        requireAuth: true,
      }),
    onSuccess: (data) => {
      queryClient.setQueryData(['infrastructure', 'state', false], data);
      void queryClient.invalidateQueries({ queryKey: ['infrastructure'] });
    },
  });
}

export function useRunNodeAction() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({
      nodeId,
      step,
    }: {
      nodeId: string;
      step: 'ssh' | 'monitor' | 'api' | 'vpn' | 'wireguard';
    }) =>
      apiRequest<{ node: InfraNode; progress: NodeProgress }>(
        `${BASE}/nodes/${encodeURIComponent(nodeId)}/actions/${encodeURIComponent(step)}`,
        {
          method: 'POST',
          body: {},
          requireAuth: true,
        }
      ),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['infrastructure'] });
    },
  });
}

export function useSaveInfraRule() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({
      ruleId,
      revision,
      rule,
    }: {
      ruleId: string;
      revision: number;
      rule: InfraRule;
    }) =>
      apiRequest<InfrastructureStateResponse>(`${BASE}/rules/${encodeURIComponent(ruleId)}`, {
        method: 'PUT',
        body: { revision, rule },
        requireAuth: true,
      }),
    onSuccess: (data) => {
      queryClient.setQueryData(['infrastructure', 'state', false], data);
      void queryClient.invalidateQueries({ queryKey: ['infrastructure'] });
    },
  });
}

export function useDeleteInfraRule() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ ruleId, revision }: { ruleId: string; revision: number }) =>
      apiRequest<InfrastructureStateResponse>(`${BASE}/rules/${encodeURIComponent(ruleId)}`, {
        method: 'DELETE',
        body: { revision },
        requireAuth: true,
      }),
    onSuccess: (data) => {
      queryClient.setQueryData(['infrastructure', 'state', false], data);
      void queryClient.invalidateQueries({ queryKey: ['infrastructure'] });
    },
  });
}

export function useExplainRoute() {
  return useMutation({
    mutationFn: (input: {
      method: string;
      path: string;
      model?: string;
      modelHint?: string;
      source?: 'draft' | 'active' | 'production';
    }) =>
      apiRequest<ExplainRouteResponse>(`${BASE}/explain`, {
        method: 'POST',
        body: input,
        requireAuth: true,
      }),
  });
}

export function usePreviewInfraRelease() {
  return useMutation({
    mutationFn: (input: { revision: number }) =>
      apiRequest<ReleasePreviewResponse>(`${BASE}/releases/preview`, {
        method: 'POST',
        body: input,
        requireAuth: true,
      }),
  });
}

export function useCreateInfraRelease() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: { revision: number; baseVersion: string; note: string }) =>
      apiRequest<InfraVersion>(`${BASE}/releases`, {
        method: 'POST',
        body: input,
        requireAuth: true,
      }),
    onSuccess: (created) => {
      queryClient.setQueryData<InfrastructureStateResponse>(
        ['infrastructure', 'state', false],
        (prev) => {
          if (!prev) return prev;
          const exists = prev.versions.some((v) => v.id === created.id);
          return {
            ...prev,
            versions: exists ? prev.versions : [created, ...prev.versions],
          };
        }
      );
      void queryClient.invalidateQueries({ queryKey: ['infrastructure'] });
    },
  });
}

export function useActivateInfraRelease() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: { releaseId: string; baseVersion: string }) =>
      apiRequest<InfraVersion>(
        `${BASE}/releases/${encodeURIComponent(input.releaseId)}/activate`,
        {
          method: 'POST',
          body: { baseVersion: input.baseVersion },
          requireAuth: true,
        }
      ),
    onSuccess: (activated) => {
      queryClient.setQueryData<InfrastructureStateResponse>(
        ['infrastructure', 'state', false],
        (prev) => {
          if (!prev) return prev;
          return {
            ...prev,
            activeVersion: activated.id,
            versions: prev.versions.map((v) =>
              v.id === activated.id
                ? { ...v, ...activated, status: 'active' }
                : v.status === 'active'
                  ? { ...v, status: 'superseded' }
                  : v
            ),
          };
        }
      );
      void queryClient.invalidateQueries({ queryKey: ['infrastructure'] });
    },
  });
}
