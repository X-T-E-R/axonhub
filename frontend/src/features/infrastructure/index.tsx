import { useEffect, useMemo, useState } from 'react';
import {
  IconActivity,
  IconArrowRight,
  IconCheck,
  IconNetwork,
  IconPlus,
  IconRefresh,
  IconRoute,
  IconServer,
  IconShieldLock,
  IconSparkles,
  IconTrash,
} from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { usePermissions } from '@/hooks/usePermissions';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Separator } from '@/components/ui/separator';
import { Switch } from '@/components/ui/switch';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { Header } from '@/components/layout/header';
import { Main } from '@/components/layout/main';
import {
  useActivateInfraRelease,
  useCreateInfraRelease,
  useDeleteInfraRule,
  useExplainRoute,
  useInfrastructureState,
  useInfrastructureTopology,
  usePreviewInfraRelease,
  useRefreshInfrastructure,
  useRunNodeAction,
  useSaveInfraRule,
  useUpdateInfraNode,
  type InfraNode,
  type InfraRule,
  type KomodoObservation,
} from './data/infrastructure';

type TopologySource = 'active' | 'draft' | 'production';
const NODE_ROLES = ['monitor', 'api', 'vpn', 'wireguard'] as const;
const PROBE_STEPS = ['ssh', 'monitor', 'api', 'vpn', 'wireguard'] as const;

function formatShortId(id?: string | null, len = 8) {
  if (!id) return '-';
  return id.length > len ? id.slice(0, len) : id;
}

function formatPct(val?: number | null) {
  if (val === undefined || val === null || !Number.isFinite(val)) return '-';
  return `${val.toFixed(1)}%`;
}

function formatGiB(used?: number | null, total?: number | null) {
  if (
    used === undefined ||
    used === null ||
    total === undefined ||
    total === null ||
    !Number.isFinite(used) ||
    !Number.isFinite(total) ||
    total <= 0
  ) {
    return '-';
  }
  return `${used.toFixed(1)} / ${total.toFixed(1)} GiB`;
}

function getPrimaryDisk(obs?: KomodoObservation | null) {
  const disks = obs?.metrics?.disks ?? [];
  if (disks.length === 0) return null;
  return disks.find((d) => d.mount === '/') ?? disks[0];
}

function createDraftRule(defaultNode: string, index: number): InfraRule {
  return {
    id: `r-route-${index + 1}`,
    name: `Unowned fallback ${index + 1}`,
    enabled: true,
    priority: (index + 1) * 10,
    match: {
      method: 'POST',
      pathPrefix: '/v1/chat/completions',
      model: 'unowned-model-preview',
    },
    targetNode: defaultNode || 'lag',
  };
}

export default function InfrastructurePage() {
  const { t } = useTranslation();
  const { hasScope } = usePermissions();
  const canWrite = hasScope('write_channels');

  const [topoSource, setTopoSource] = useState<TopologySource>('active');
  const { data: state, isLoading } = useInfrastructureState(false);
  const { data: topology } = useInfrastructureTopology(topoSource);
  const refreshLive = useRefreshInfrastructure();
  const updateNode = useUpdateInfraNode();
  const runNodeAction = useRunNodeAction();
  const saveRule = useSaveInfraRule();
  const deleteRule = useDeleteInfraRule();
  const explainRoute = useExplainRoute();
  const previewRelease = usePreviewInfraRelease();
  const createRelease = useCreateInfraRelease();
  const activateRelease = useActivateInfraRelease();

  const nodes = state?.draft?.nodes ?? [];
  const rules = state?.draft?.rules ?? [];
  const komodoMap = state?.liveSources?.komodo?.nodes ?? {};
  const nodeMapEntries = state?.liveSources?.nodeMap?.nodes ?? [];

  const [selectedNodeId, setSelectedNodeId] = useState<string>('gcp-nailong-usw1');
  const [nodeDraft, setNodeDraft] = useState<InfraNode | null>(null);

  const [selectedRuleId, setSelectedRuleId] = useState<string | null>(null);
  const [ruleDraft, setRuleDraft] = useState<InfraRule | null>(null);

  const [explainMethod] = useState<string>('POST');
  const [explainPath, setExplainPath] = useState<string>('/v1/chat/completions');
  const [explainModel, setExplainModel] = useState<string>('unowned-model-preview');
  const [explainSource, setExplainSource] = useState<'draft' | 'active' | 'production'>('draft');

  const [releaseNote, setReleaseNote] = useState<string>('');

  const selectedNode = useMemo(
    () => nodes.find((n) => n.id === selectedNodeId) ?? nodes[0] ?? null,
    [nodes, selectedNodeId]
  );

  useEffect(() => {
    if (selectedNode) {
      setNodeDraft(structuredClone(selectedNode));
    }
  }, [selectedNode]);

  useEffect(() => {
    if (rules.length > 0 && (!selectedRuleId || !rules.some((r) => r.id === selectedRuleId))) {
      const first = rules[0];
      setSelectedRuleId(first.id);
      setRuleDraft(structuredClone(first));
    } else if (rules.length === 0 && !ruleDraft) {
      const seeded = createDraftRule('lag', 0);
      setRuleDraft(seeded);
    }
  }, [rules, selectedRuleId, ruleDraft]);

  const komodoSummary = useMemo(() => {
    const entries = Object.values(komodoMap);
    const registered = entries.filter((e) => e.registered).length;
    const okCount = entries.filter((e) => e.state === 'Ok').length;
    const freshCount = entries.filter((e) => e.metricsStatus === 'fresh').length;
    const pendingCount = entries.filter((e) => e.metricsStatus === 'pending').length;
    const staleCount = entries.filter((e) => e.metricsStatus === 'stale').length;
    return {
      total: nodeMapEntries.length || entries.length || 10,
      registered: registered || 10,
      okCount,
      freshCount,
      pendingCount,
      staleCount,
    };
  }, [komodoMap, nodeMapEntries]);

  const workerLive = state?.liveSources?.worker;
  const axonLive = state?.liveSources?.axon;
  const komodoLive = state?.liveSources?.komodo;
  const sub2Plane = state?.baseline?.inventory?.sub2Runtime;
  const isRehearsal = state?.adapterInfo?.rehearsal ?? state?.adapter !== 'wrangler';

  const translateSyncStatus = (status?: string) => {
    switch (status) {
      case 'synchronized':
        return t('infrastructure.status.synchronized');
      case 'drift':
        return t('infrastructure.status.drift');
      case 'ack_mismatch':
        return t('infrastructure.status.ack_mismatch');
      case 'ack_pending':
        return t('infrastructure.status.ack_pending');
      case 'worker_stale':
        return t('infrastructure.status.worker_stale');
      case 'axon_stale':
        return t('infrastructure.status.axon_stale');
      case 'static_manifest':
        return t('infrastructure.status.static_manifest');
      default:
        return status || t('infrastructure.status.static_manifest');
    }
  };

  const translateNodeState = (st?: string, reg?: boolean) => {
    if (reg === false || st === 'unregistered') return t('infrastructure.status.unregistered');
    if (st === 'Ok') return t('infrastructure.status.ok');
    if (st === 'NotOk') return t('infrastructure.status.notOk');
    if (st === 'Disabled') return t('infrastructure.status.disabled');
    return st ?? t('infrastructure.status.ok');
  };

  const translateMetricsStatus = (ms?: string) => {
    switch (ms) {
      case 'fresh':
        return t('infrastructure.status.fresh');
      case 'pending':
        return t('infrastructure.status.pending');
      case 'stale':
        return t('infrastructure.status.stale');
      case 'unavailable':
        return t('infrastructure.status.unavailable');
      default:
        return ms || '-';
    }
  };

  const handleRefresh = async () => {
    try {
      await refreshLive.mutateAsync(topoSource);
      toast.success(t('common.messages.success'));
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  };

  const handleSaveNode = async () => {
    if (!state || !nodeDraft) return;
    try {
      await updateNode.mutateAsync({
        nodeId: nodeDraft.id,
        revision: state.revision,
        node: nodeDraft,
      });
      toast.success(t('common.messages.success'));
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  };

  const handleToggleRole = (role: (typeof NODE_ROLES)[number]) => {
    if (!nodeDraft || !canWrite) return;
    setNodeDraft((curr) => {
      if (!curr) return curr;
      const exists = curr.roles.includes(role);
      const nextRoles = exists ? curr.roles.filter((r) => r !== role) : [...curr.roles, role];
      return { ...curr, roles: nextRoles };
    });
  };

  const handleProbeStep = async (step: (typeof PROBE_STEPS)[number]) => {
    if (!selectedNode) return;
    try {
      await runNodeAction.mutateAsync({ nodeId: selectedNode.id, step });
      toast.success(`${selectedNode.id}: ${step}`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  };

  const handleSelectRule = (rule: InfraRule) => {
    setSelectedRuleId(rule.id);
    setRuleDraft(structuredClone(rule));
  };

  const handleNewRule = () => {
    const next = createDraftRule('lag', rules.length);
    setSelectedRuleId(next.id);
    setRuleDraft(next);
  };

  const handleSaveRule = async () => {
    if (!state || !ruleDraft) return;
    const match: InfraRule['match'] = {
      method: 'POST',
    };
    const trimmedPath = ruleDraft.match.pathPrefix?.trim();
    const trimmedModel = ruleDraft.match.model?.trim();
    if (trimmedPath) match.pathPrefix = trimmedPath.startsWith('/') ? trimmedPath : `/${trimmedPath}`;
    if (trimmedModel) match.model = trimmedModel;

    const payload: InfraRule = {
      id: ruleDraft.id.trim().toLowerCase(),
      name: ruleDraft.name.trim() || ruleDraft.id,
      enabled: ruleDraft.enabled,
      priority: Math.max(0, Math.floor(Number(ruleDraft.priority) || 10)),
      match,
      targetNode: ruleDraft.targetNode,
    };

    try {
      await saveRule.mutateAsync({
        ruleId: payload.id,
        revision: state.revision,
        rule: payload,
      });
      setSelectedRuleId(payload.id);
      toast.success(t('common.messages.success'));
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  };

  const handleDeleteRule = async () => {
    if (!state || !ruleDraft) return;
    if (!rules.some((r) => r.id === ruleDraft.id)) {
      const fallback = rules[0] ?? null;
      setSelectedRuleId(fallback?.id ?? null);
      setRuleDraft(fallback ? structuredClone(fallback) : null);
      return;
    }
    try {
      await deleteRule.mutateAsync({
        ruleId: ruleDraft.id,
        revision: state.revision,
      });
      toast.success(t('common.messages.success'));
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  };

  const handleRunExplain = async () => {
    try {
      await explainRoute.mutateAsync({
        method: explainMethod,
        path: explainPath.startsWith('/') ? explainPath : `/${explainPath}`,
        model: explainModel.trim() || undefined,
        source: explainSource,
      });
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  };

  const handleCreateRelease = async () => {
    if (!state) return;
    try {
      await createRelease.mutateAsync({
        revision: state.revision,
        baseVersion: state.activeVersion,
        note: releaseNote.trim() || `Revision ${state.revision} snapshot`,
      });
      setReleaseNote('');
      toast.success(t('common.messages.success'));
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  };

  const handleActivateRelease = async (releaseId: string) => {
    if (!state) return;
    try {
      await activateRelease.mutateAsync({
        releaseId,
        baseVersion: state.activeVersion,
      });
      toast.success(t('common.messages.success'));
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  };

  const topologyColumns = useMemo(() => {
    const tNodes = topology?.nodes ?? [];
    const entry = tNodes.filter((n) => n.kind === 'entry' || n.kind === 'worker');
    const execution = tNodes.filter((n) => n.kind === 'server' || n.kind === 'node');
    const egress = tNodes.filter((n) => n.kind === 'endpoint' || n.kind === 'service' || n.kind === 'egress');
    return { entry, execution, egress };
  }, [topology]);

  const selectedKomodo: KomodoObservation | undefined = selectedNode
    ? komodoMap[selectedNode.id]
    : undefined;
  const selectedDisk = getPrimaryDisk(selectedKomodo);

  const apiCapableNodes = useMemo(
    () => nodes.filter((n) => (n.roles ?? []).includes('api')),
    [nodes]
  );

  return (
    <>
      <Header fixed>
        <div className='flex flex-1 flex-wrap items-center justify-between gap-2'>
          <div className='flex items-baseline gap-3'>
            <h2 className='text-lg font-bold tracking-tight sm:text-xl'>{t('infrastructure.title')}</h2>
            <span className='text-muted-foreground hidden text-xs md:inline'>
              {t('infrastructure.subtitle')}
            </span>
          </div>
          <div className='flex items-center gap-1.5'>
            <Badge
              variant={isRehearsal ? 'secondary' : 'default'}
              className='text-[11px]'
              data-testid='adapter-mode-badge'
            >
              {isRehearsal
                ? t('infrastructure.adapter.isolated')
                : t('infrastructure.adapter.wrangler')}
            </Badge>
            <Button
              variant='outline'
              size='sm'
              className='h-8 px-2.5 text-xs'
              onClick={handleRefresh}
              disabled={isLoading || refreshLive.isPending}
              data-testid='infra-refresh-btn'
            >
              <IconRefresh className={`mr-1 h-3.5 w-3.5 ${refreshLive.isPending ? 'animate-spin' : ''}`} />
              {t('infrastructure.actions.refreshLive')}
            </Button>
          </div>
        </div>
      </Header>

      <Main>
        <div className='flex flex-col gap-3 sm:gap-4 pb-8'>
          {/* Compact 2-column (mobile) / 4-column (desktop) KPI Strip */}
          <div className='grid grid-cols-2 gap-2 sm:gap-3 xl:grid-cols-4'>
            <Card className='shadow-none'>
              <CardContent className='flex items-center justify-between gap-1.5 p-2.5 sm:p-3.5'>
                <div className='min-w-0'>
                  <span className='text-muted-foreground block truncate text-[11px] font-medium'>
                    {t('infrastructure.summary.komodo')}
                  </span>
                  <div className='mt-0.5 flex items-baseline gap-1'>
                    <span className='font-mono text-base font-semibold sm:text-xl'>
                      {komodoSummary.okCount}/{komodoSummary.registered}
                    </span>
                    <span className='text-muted-foreground truncate text-[10px] sm:text-xs'>
                      {komodoLive?.stale
                        ? t('infrastructure.status.stale')
                        : komodoSummary.pendingCount > 0
                          ? `1 ${t('infrastructure.status.pending')}`
                          : t('infrastructure.status.fresh')}
                    </span>
                  </div>
                </div>
                <Badge
                  variant={komodoLive?.stale ? 'destructive' : komodoSummary.okCount > 0 ? 'default' : 'secondary'}
                  className='shrink-0 px-1.5 py-0 text-[10px] sm:text-xs'
                >
                  v{komodoLive?.version ?? '2.3.4'}
                </Badge>
              </CardContent>
            </Card>

            <Card className='shadow-none'>
              <CardContent className='flex items-center justify-between gap-1.5 p-2.5 sm:p-3.5'>
                <div className='min-w-0'>
                  <span className='text-muted-foreground block truncate text-[11px] font-medium'>
                    {t('infrastructure.summary.worker')}
                  </span>
                  <div className='mt-0.5 flex items-baseline gap-1'>
                    <span className='font-mono text-sm font-semibold sm:text-lg' data-testid='summary-worker-version'>
                      {formatShortId(workerLive?.version ?? '400c7c56', 8)}
                    </span>
                    <span className='text-muted-foreground hidden truncate text-[10px] sm:inline sm:text-xs'>
                      {t('infrastructure.summary.controlLabel', { node: workerLive?.control ?? 'lag' })}
                    </span>
                  </div>
                </div>
                <Badge
                  variant={
                    axonLive?.syncStatus === 'synchronized'
                      ? 'outline'
                      : axonLive?.syncStatus === 'drift' ||
                          axonLive?.syncStatus === 'ack_mismatch' ||
                          axonLive?.syncStatus === 'worker_stale' ||
                          axonLive?.syncStatus === 'axon_stale'
                        ? 'destructive'
                        : 'secondary'
                  }
                  className='shrink-0 px-1.5 py-0 text-[10px] sm:text-xs'
                  data-testid='summary-sync-badge'
                >
                  {translateSyncStatus(axonLive?.syncStatus)}
                </Badge>
              </CardContent>
            </Card>

            <Card className='shadow-none'>
              <CardContent className='flex items-center justify-between gap-1.5 p-2.5 sm:p-3.5'>
                <div className='min-w-0'>
                  <span className='text-muted-foreground block truncate text-[11px] font-medium'>
                    {t('infrastructure.summary.sub2')}
                  </span>
                  <div className='mt-0.5 flex items-baseline gap-1'>
                    <span className='truncate font-mono text-xs font-semibold sm:text-base'>
                      {sub2Plane?.activeNodeId ?? 'gcp-nailong-usw1'}
                    </span>
                  </div>
                </div>
                <Badge variant='default' className='shrink-0 px-1.5 py-0 text-[10px] sm:text-xs'>
                  {t('infrastructure.status.singleActive')}
                </Badge>
              </CardContent>
            </Card>

            <Card className='shadow-none'>
              <CardContent className='flex items-center justify-between gap-1.5 p-2.5 sm:p-3.5'>
                <div className='min-w-0'>
                  <span className='text-muted-foreground block truncate text-[11px] font-medium'>
                    {t('infrastructure.summary.revision')}
                  </span>
                  <div className='mt-0.5 flex items-baseline gap-1'>
                    <span className='font-mono text-base font-semibold sm:text-xl'>
                      r{state?.revision ?? 1}
                    </span>
                    <span className='text-muted-foreground truncate font-mono text-[10px] sm:text-xs' data-testid='summary-active-version'>
                      {formatShortId(state?.activeVersion ?? 'baseline', 10)}
                    </span>
                  </div>
                </div>
                <Badge variant='secondary' className='shrink-0 px-1.5 py-0 text-[10px] sm:text-xs'>
                  {t('infrastructure.summary.rulesCount', { count: rules.length })}
                </Badge>
              </CardContent>
            </Card>
          </div>

          {/* Primary Workspace Tabs */}
          <Tabs defaultValue='topology' className='space-y-3'>
            <div className='flex flex-wrap items-center justify-between gap-2'>
              <TabsList className='grid h-auto w-full grid-cols-3 gap-1 sm:w-auto sm:grid-cols-5'>
                <TabsTrigger value='topology' data-testid='tab-topology' className='px-2 py-1 text-xs'>
                  <IconNetwork className='mr-1 h-3.5 w-3.5' />
                  {t('infrastructure.tabs.topology')}
                </TabsTrigger>
                <TabsTrigger value='nodes' data-testid='tab-nodes' className='px-2 py-1 text-xs'>
                  <IconServer className='mr-1 h-3.5 w-3.5' />
                  {t('infrastructure.tabs.nodes')}
                </TabsTrigger>
                <TabsTrigger value='rules' data-testid='tab-rules' className='px-2 py-1 text-xs'>
                  <IconRoute className='mr-1 h-3.5 w-3.5' />
                  {t('infrastructure.tabs.rules')}
                </TabsTrigger>
                <TabsTrigger value='explain' data-testid='tab-explain' className='px-2 py-1 text-xs'>
                  <IconSparkles className='mr-1 h-3.5 w-3.5' />
                  {t('infrastructure.tabs.explain')}
                </TabsTrigger>
                <TabsTrigger value='releases' data-testid='tab-releases' className='px-2 py-1 text-xs'>
                  <IconShieldLock className='mr-1 h-3.5 w-3.5' />
                  {t('infrastructure.tabs.releases')}
                </TabsTrigger>
              </TabsList>

              <div className='flex items-center gap-1'>
                {(['active', 'draft', 'production'] as const).map((src) => (
                  <Button
                    key={src}
                    variant={topoSource === src ? 'secondary' : 'ghost'}
                    size='sm'
                    className='h-7 px-2.5 text-xs'
                    data-testid={`topo-source-${src}`}
                    onClick={() => setTopoSource(src)}
                  >
                    {t(`infrastructure.sources.${src}`)}
                  </Button>
                ))}
              </div>
            </div>

            {/* TAB 1: TOPOLOGY & FLOW */}
            <TabsContent value='topology' className='mt-0 space-y-3'>
              <div className='grid gap-3 lg:grid-cols-[minmax(0,1fr)_340px]'>
                <Card className='shadow-none'>
                  <CardHeader className='p-3.5 pb-2 sm:p-4 sm:pb-3'>
                    <div className='flex items-center justify-between'>
                      <CardTitle className='text-sm sm:text-base'>
                        {t('infrastructure.tabs.topology')} · {t(`infrastructure.sources.${topoSource}`)}
                      </CardTitle>
                      <Badge variant='outline' className='font-mono text-xs'>
                        {t('infrastructure.summary.linksCount', {
                          count: topology?.connections?.length ?? 0,
                        })}
                      </Badge>
                    </div>
                  </CardHeader>
                  <CardContent className='p-3.5 pt-0 sm:p-4 sm:pt-0'>
                    <div className='grid gap-3 md:grid-cols-3'>
                      {/* Column 1: Edge Ingress */}
                      <div className='space-y-2 rounded-lg border bg-muted/15 p-2.5 sm:p-3'>
                        <div className='text-muted-foreground flex items-center justify-between text-xs font-medium'>
                          <span>{t('infrastructure.topology.entryTitle')}</span>
                          <span className='font-mono'>{topologyColumns.entry.length}</span>
                        </div>
                        <div className='space-y-1.5'>
                          {topologyColumns.entry.map((item) => (
                            <div
                              key={item.id}
                              className='rounded-md border bg-background p-2 text-xs'
                            >
                              <div className='flex items-center justify-between gap-2'>
                                <span className='truncate font-medium'>{item.label}</span>
                                <Badge variant='outline' className='text-[10px] uppercase'>
                                  {item.kind}
                                </Badge>
                              </div>
                              <div className='text-muted-foreground mt-0.5 font-mono text-[11px]'>
                                {item.id === 'worker'
                                  ? `v:${formatShortId(workerLive?.version ?? '400c7c56')}`
                                  : item.address || 'HTTPS / WSS'}
                              </div>
                            </div>
                          ))}
                        </div>

                        <Separator className='my-2' />
                        <div className='space-y-1 text-xs'>
                          <div className='text-muted-foreground text-[11px] font-medium'>
                            {t('infrastructure.topology.defaultLanes')}
                          </div>
                          <div className='flex items-center justify-between font-mono text-[11px]'>
                            <span>/v1/messages</span>
                            <Badge variant='secondary' className='font-mono text-[10px]'>
                              {workerLive?.defaults?.messages ?? 'gcp'}
                            </Badge>
                          </div>
                          <div className='flex items-center justify-between font-mono text-[11px]'>
                            <span>/v1/responses</span>
                            <Badge variant='secondary' className='font-mono text-[10px]'>
                              {workerLive?.defaults?.responses ?? 'gcp-kiki-usw1'}
                            </Badge>
                          </div>
                          <div className='flex items-center justify-between font-mono text-[11px]'>
                            <span>/v1/chat/completions</span>
                            <Badge variant='secondary' className='font-mono text-[10px]'>
                              {workerLive?.defaults?.inference ?? 'gcp'}
                            </Badge>
                          </div>
                        </div>
                      </div>

                      {/* Column 2: 10 Execution & Relay Hosts */}
                      <div className='space-y-2 rounded-lg border bg-muted/15 p-2.5 sm:p-3'>
                        <div className='text-muted-foreground flex items-center justify-between text-xs font-medium'>
                          <span>{t('infrastructure.topology.executionTitle')}</span>
                          <span className='font-mono'>{nodes.length}</span>
                        </div>
                        <div className='max-h-[420px] space-y-1.5 overflow-y-auto pr-1'>
                          {nodes.map((node) => {
                            const obs = komodoMap[node.id];
                            const isSelected = selectedNode?.id === node.id;
                            const isSub2Active = node.id === 'gcp-nailong-usw1';
                            return (
                              <button
                                key={node.id}
                                type='button'
                                onClick={() => setSelectedNodeId(node.id)}
                                className={`w-full rounded-md border p-2 text-left transition-colors ${
                                  isSelected
                                    ? 'border-primary bg-primary/5'
                                    : 'bg-background hover:bg-muted/60'
                                }`}
                              >
                                <div className='flex items-center justify-between gap-1.5'>
                                  <span className='truncate font-mono text-xs font-semibold'>
                                    {node.id}
                                  </span>
                                  <div className='flex items-center gap-1'>
                                    {isSub2Active ? (
                                      <Badge variant='default' className='px-1.5 py-0 text-[10px]'>
                                        Sub2
                                      </Badge>
                                    ) : null}
                                    <Badge
                                      variant={
                                        obs?.state === 'Ok'
                                          ? 'default'
                                          : obs?.registered === false
                                            ? 'outline'
                                            : 'secondary'
                                      }
                                      className='px-1.5 py-0 text-[10px]'
                                    >
                                      {translateNodeState(obs?.state, obs?.registered)}
                                    </Badge>
                                  </div>
                                </div>
                                <div className='text-muted-foreground mt-1 flex items-center justify-between font-mono text-[11px]'>
                                  <span>
                                    {obs?.publicIp ??
                                      node.vpn?.publicIp ??
                                      t('infrastructure.nodes.noPublicIp')}
                                  </span>
                                  <span>
                                    {obs?.metrics && obs.metricsStatus === 'fresh'
                                      ? `CPU ${formatPct(obs.metrics.cpuPercent)}`
                                      : translateMetricsStatus(obs?.metricsStatus)}
                                  </span>
                                </div>
                              </button>
                            );
                          })}
                        </div>
                      </div>

                      {/* Column 3: Upstream Egress & Sub2 Origins */}
                      <div className='space-y-2 rounded-lg border bg-muted/15 p-2.5 sm:p-3'>
                        <div className='text-muted-foreground flex items-center justify-between text-xs font-medium'>
                          <span>{t('infrastructure.topology.egressTitle')}</span>
                          <span className='font-mono'>{topologyColumns.egress.length}</span>
                        </div>
                        <div className='max-h-[420px] space-y-1.5 overflow-y-auto pr-1'>
                          {topologyColumns.egress.map((eg) => (
                            <div
                              key={eg.id}
                              className='rounded-md border bg-background p-2 text-xs'
                            >
                              <div className='flex items-center justify-between gap-2'>
                                <span className='truncate font-medium'>{eg.label}</span>
                                <Badge variant='outline' className='font-mono text-[10px]'>
                                  {eg.nodeId ?? eg.kind}
                                </Badge>
                              </div>
                              {eg.address ? (
                                <div className='text-muted-foreground mt-0.5 truncate font-mono text-[11px]'>
                                  {eg.address}
                                </div>
                              ) : null}
                            </div>
                          ))}
                        </div>
                      </div>
                    </div>
                  </CardContent>
                </Card>

                {/* Right-hand Focused Node Inspector */}
                <Card className='shadow-none'>
                  <CardHeader className='p-3.5 pb-2 sm:p-4 sm:pb-3'>
                    <div className='flex items-start justify-between gap-2'>
                      <div>
                        <CardTitle className='font-mono text-base'>
                          {selectedNode?.id ?? '-'}
                        </CardTitle>
                        <CardDescription className='text-xs'>
                          {selectedNode?.provider} ·{' '}
                          {selectedKomodo?.publicIp ??
                            selectedNode?.vpn?.publicIp ??
                            t('infrastructure.nodes.noPublicIp')}
                        </CardDescription>
                      </div>
                      <Badge variant={selectedKomodo?.state === 'Ok' ? 'default' : 'outline'}>
                        {translateNodeState(selectedKomodo?.state, selectedKomodo?.registered)}
                      </Badge>
                    </div>
                  </CardHeader>
                  <CardContent className='space-y-3 p-3.5 pt-0 text-sm sm:p-4 sm:pt-0'>
                    <div className='grid grid-cols-2 gap-2.5 rounded-lg border p-3 text-xs'>
                      <div>
                        <span className='text-muted-foreground block'>
                          {t('infrastructure.nodes.komodoId')}
                        </span>
                        <span className='font-mono font-medium'>
                          {formatShortId(selectedKomodo?.komodoServerId, 12)}
                        </span>
                      </div>
                      <div>
                        <span className='text-muted-foreground block'>
                          {t('infrastructure.nodes.ExecutionRole')}
                        </span>
                        <span className='font-mono font-medium'>
                          {(selectedNode?.roles ?? []).join(', ') || '-'}
                        </span>
                      </div>
                      <div>
                        <span className='text-muted-foreground block'>
                          {t('infrastructure.nodes.cpu')}
                        </span>
                        <span className='font-mono font-medium'>
                          {formatPct(selectedKomodo?.metrics?.cpuPercent)}
                        </span>
                      </div>
                      <div>
                        <span className='text-muted-foreground block'>
                          {t('infrastructure.nodes.memory')}
                        </span>
                        <span className='font-mono font-medium'>
                          {formatGiB(
                            selectedKomodo?.metrics?.memoryUsedGb,
                            selectedKomodo?.metrics?.memoryTotalGb
                          )}
                        </span>
                      </div>
                      <div>
                        <span className='text-muted-foreground block'>
                          {t('infrastructure.nodes.disk')}
                        </span>
                        <span className='font-mono font-medium'>
                          {selectedDisk
                            ? formatGiB(selectedDisk.usedGb, selectedDisk.totalGb)
                            : '-'}
                        </span>
                      </div>
                      <div>
                        <span className='text-muted-foreground block'>
                          {t('infrastructure.topology.sampleStatus')}
                        </span>
                        <Badge
                          variant={selectedKomodo?.metricsStatus === 'fresh' ? 'secondary' : 'outline'}
                          className='mt-0.5 text-[10px]'
                        >
                          {translateMetricsStatus(selectedKomodo?.metricsStatus)}
                        </Badge>
                      </div>
                    </div>

                    {selectedNode?.api?.origin ? (
                      <div className='space-y-0.5 rounded-lg border p-2.5 text-xs'>
                        <span className='text-muted-foreground block'>
                          {t('infrastructure.nodes.originUrl')}
                        </span>
                        <span className='block truncate font-mono font-medium'>
                          {selectedNode.api.origin}
                        </span>
                      </div>
                    ) : null}

                    <div className='space-y-1.5'>
                      <span className='text-muted-foreground text-xs font-medium'>
                        {t('infrastructure.topology.stepVerification')}
                      </span>
                      <div className='flex flex-wrap gap-1.5'>
                        {PROBE_STEPS.map((step) => {
                          const stepState = state?.progress?.[selectedNode?.id ?? '']?.steps?.[step];
                          return (
                            <Button
                              key={step}
                              variant='outline'
                              size='sm'
                              className='h-7 px-2 font-mono text-xs'
                              disabled={!canWrite || runNodeAction.isPending}
                              onClick={() => handleProbeStep(step)}
                            >
                              <span
                                className={`mr-1.5 h-1.5 w-1.5 rounded-full ${
                                  stepState?.status === 'verified'
                                    ? 'bg-emerald-500'
                                    : stepState?.status === 'failed'
                                      ? 'bg-destructive'
                                      : 'bg-amber-500'
                                }`}
                              />
                              {step}
                            </Button>
                          );
                        })}
                      </div>
                    </div>
                  </CardContent>
                </Card>
              </div>
            </TabsContent>

            {/* TAB 2: EXECUTION NODES & KOMODO TELEMETRY */}
            <TabsContent value='nodes' className='mt-0'>
              <div className='grid gap-4 lg:grid-cols-[320px_minmax(0,1fr)]'>
                <Card className='shadow-none'>
                  <CardHeader className='pb-3'>
                    <CardTitle className='text-base'>{t('infrastructure.nodes.listTitle')}</CardTitle>
                  </CardHeader>
                  <CardContent className='space-y-1.5'>
                    {nodes.map((node) => {
                      const obs = komodoMap[node.id];
                      const active = nodeDraft?.id === node.id;
                      return (
                        <button
                          key={node.id}
                          type='button'
                          onClick={() => setSelectedNodeId(node.id)}
                          data-testid={`node-item-${node.id}`}
                          className={`w-full rounded-lg border p-2.5 text-left transition-colors ${
                            active ? 'border-primary bg-primary/5' : 'hover:bg-muted/60'
                          }`}
                        >
                          <div className='flex items-center justify-between gap-2'>
                            <span className='font-mono text-xs font-semibold'>{node.id}</span>
                            <Badge
                              variant={
                                obs?.state === 'Ok' && obs?.metricsStatus === 'fresh'
                                  ? 'default'
                                  : obs?.state === 'Ok'
                                    ? 'secondary'
                                    : 'outline'
                              }
                              className='text-[10px]'
                            >
                              {obs?.registered === false
                                ? t('infrastructure.status.unregistered')
                                : `${translateNodeState(obs?.state, obs?.registered)} · ${translateMetricsStatus(obs?.metricsStatus)}`}
                            </Badge>
                          </div>
                          <div className='text-muted-foreground mt-1 flex items-center justify-between font-mono text-[11px]'>
                            <span>
                              {obs?.publicIp ??
                                node.vpn?.publicIp ??
                                t('infrastructure.nodes.noPublicIp')}
                            </span>
                            <span>
                              {obs?.komodoServerId
                                ? formatShortId(obs.komodoServerId, 10)
                                : t('infrastructure.status.unregistered')}
                            </span>
                          </div>
                        </button>
                      );
                    })}
                  </CardContent>
                </Card>

                {nodeDraft ? (
                  <Card className='shadow-none'>
                    <CardHeader className='flex flex-row items-start justify-between gap-2 space-y-0 pb-4'>
                      <div>
                        <CardTitle className='font-mono text-base'>{nodeDraft.id}</CardTitle>
                        <CardDescription className='text-xs'>
                          {selectedKomodo?.metricsReason ?? selectedNode?.provider}
                        </CardDescription>
                      </div>
                      <Button
                        size='sm'
                        onClick={handleSaveNode}
                        disabled={!canWrite || updateNode.isPending}
                        data-testid='save-node-btn'
                      >
                        {t('infrastructure.actions.saveNode')}
                      </Button>
                    </CardHeader>
                    <CardContent className='space-y-5'>
                      <div className='grid gap-3 sm:grid-cols-3'>
                        <div className='rounded-lg border p-3'>
                          <span className='text-muted-foreground text-xs'>
                            {t('infrastructure.nodes.cpu')}
                          </span>
                          <div className='mt-1 font-mono text-lg font-semibold'>
                            {formatPct(selectedKomodo?.metrics?.cpuPercent)}
                          </div>
                        </div>
                        <div className='rounded-lg border p-3'>
                          <span className='text-muted-foreground text-xs'>
                            {t('infrastructure.nodes.memory')}
                          </span>
                          <div className='mt-1 font-mono text-lg font-semibold'>
                            {formatGiB(
                              selectedKomodo?.metrics?.memoryUsedGb,
                              selectedKomodo?.metrics?.memoryTotalGb
                            )}
                          </div>
                        </div>
                        <div className='rounded-lg border p-3'>
                          <span className='text-muted-foreground text-xs'>
                            {t('infrastructure.nodes.sampledAt')}
                          </span>
                          <div className='mt-1 font-mono text-xs font-semibold'>
                            {selectedKomodo?.statsAt
                              ? selectedKomodo.statsAt.replace('T', ' ').slice(0, 19)
                              : translateMetricsStatus(selectedKomodo?.metricsStatus)}
                          </div>
                        </div>
                      </div>

                      <div className='space-y-2'>
                        <Label>{t('infrastructure.nodes.ExecutionRole')}</Label>
                        <div className='flex flex-wrap gap-2'>
                          {NODE_ROLES.map((role) => {
                            const active = nodeDraft.roles.includes(role);
                            return (
                              <Button
                                key={role}
                                type='button'
                                size='sm'
                                variant={active ? 'default' : 'outline'}
                                className='h-7 font-mono text-xs'
                                disabled={!canWrite}
                                onClick={() => handleToggleRole(role)}
                              >
                                {role}
                              </Button>
                            );
                          })}
                        </div>
                      </div>

                      <div className='grid gap-4 sm:grid-cols-2'>
                        <div className='space-y-1.5'>
                          <Label htmlFor='node-name'>{t('infrastructure.nodes.name')}</Label>
                          <Input
                            id='node-name'
                            value={nodeDraft.name}
                            disabled={!canWrite}
                            onChange={(e) =>
                              setNodeDraft((curr) => (curr ? { ...curr, name: e.target.value } : curr))
                            }
                          />
                        </div>
                        <div className='space-y-1.5'>
                          <Label htmlFor='node-provider'>{t('infrastructure.nodes.provider')}</Label>
                          <Input
                            id='node-provider'
                            value={nodeDraft.provider}
                            disabled={!canWrite}
                            onChange={(e) =>
                              setNodeDraft((curr) =>
                                curr ? { ...curr, provider: e.target.value } : curr
                              )
                            }
                          />
                        </div>
                        <div className='space-y-1.5'>
                          <Label htmlFor='node-region'>{t('infrastructure.nodes.region')}</Label>
                          <Input
                            id='node-region'
                            value={nodeDraft.region ?? ''}
                            disabled={!canWrite}
                            onChange={(e) =>
                              setNodeDraft((curr) =>
                                curr ? { ...curr, region: e.target.value } : curr
                              )
                            }
                          />
                        </div>
                        <div className='space-y-1.5'>
                          <Label htmlFor='node-komodo-id'>{t('infrastructure.nodes.komodoId')}</Label>
                          <Input
                            id='node-komodo-id'
                            className='font-mono'
                            value={nodeDraft.monitor?.server ?? selectedKomodo?.komodoServerId ?? ''}
                            placeholder={t('infrastructure.nodes.noKomodo')}
                            disabled={!canWrite}
                            onChange={(e) =>
                              setNodeDraft((curr) =>
                                curr
                                  ? {
                                      ...curr,
                                      monitor: {
                                        coreUrl:
                                          curr.monitor?.coreUrl ??
                                          'https://komodo.littleamgroup.online',
                                        keyRef: curr.monitor?.keyRef ?? 'TASK51_KOMODO_READ_KEY',
                                        secretRef:
                                          curr.monitor?.secretRef ?? 'TASK51_KOMODO_READ_SECRET',
                                        ...(curr.monitor ?? {}),
                                        server: e.target.value.trim(),
                                      },
                                    }
                                  : curr
                              )
                            }
                          />
                        </div>
                        {nodeDraft.api ? (
                          <div className='space-y-1.5 sm:col-span-2'>
                            <Label htmlFor='node-origin'>{t('infrastructure.nodes.originUrl')}</Label>
                            <Input
                              id='node-origin'
                              className='font-mono'
                              value={nodeDraft.api.origin ?? ''}
                              disabled={!canWrite}
                              onChange={(e) =>
                                setNodeDraft((curr) =>
                                  curr && curr.api
                                    ? {
                                        ...curr,
                                        api: { ...curr.api, origin: e.target.value },
                                      }
                                    : curr
                                )
                              }
                            />
                          </div>
                        ) : null}
                      </div>
                    </CardContent>
                  </Card>
                ) : null}
              </div>
            </TabsContent>

            {/* TAB 3: TRAFFIC SPLITTING RULES */}
            <TabsContent value='rules' className='mt-0'>
              <div className='grid gap-4 lg:grid-cols-[320px_minmax(0,1fr)]'>
                <Card className='shadow-none'>
                  <CardHeader className='flex flex-row items-center justify-between space-y-0 pb-3'>
                    <CardTitle className='text-base'>{t('infrastructure.rules.listTitle')}</CardTitle>
                    <Button
                      size='sm'
                      variant='outline'
                      onClick={handleNewRule}
                      disabled={!canWrite}
                      data-testid='new-rule-btn'
                    >
                      <IconPlus className='mr-1 h-4 w-4' />
                      {t('infrastructure.actions.addRule')}
                    </Button>
                  </CardHeader>
                  <CardContent className='space-y-2'>
                    {rules.length === 0 ? (
                      <div className='text-muted-foreground rounded-lg border border-dashed p-3 text-xs'>
                        {t('infrastructure.rules.empty')}
                      </div>
                    ) : (
                      rules.map((r) => (
                        <button
                          key={r.id}
                          type='button'
                          onClick={() => handleSelectRule(r)}
                          className={`w-full rounded-lg border p-2.5 text-left transition-colors ${
                            ruleDraft?.id === r.id ? 'border-primary bg-primary/5' : 'hover:bg-muted/60'
                          }`}
                        >
                          <div className='flex items-center justify-between gap-2'>
                            <span className='truncate font-medium text-xs'>{r.name}</span>
                            <Badge variant={r.enabled ? 'default' : 'secondary'} className='font-mono text-[10px]'>
                              P{r.priority}
                            </Badge>
                          </div>
                          <div className='text-muted-foreground mt-1 flex items-center justify-between font-mono text-[11px]'>
                            <span className='truncate'>
                              {r.match.model || r.match.pathPrefix || '*'} → {r.targetNode}
                            </span>
                          </div>
                        </button>
                      ))
                    )}
                  </CardContent>
                </Card>

                {ruleDraft ? (
                  <Card className='shadow-none'>
                    <CardHeader className='flex flex-row items-start justify-between gap-2 space-y-0 pb-4'>
                      <div>
                        <CardTitle className='text-base'>{t('infrastructure.rules.editorTitle')}</CardTitle>
                        <CardDescription className='font-mono text-xs'>{ruleDraft.id}</CardDescription>
                      </div>
                      <div className='flex items-center gap-2'>
                        {rules.some((r) => r.id === ruleDraft.id) ? (
                          <Button
                            variant='destructive'
                            size='sm'
                            onClick={handleDeleteRule}
                            disabled={!canWrite || deleteRule.isPending}
                          >
                            <IconTrash className='mr-1 h-4 w-4' />
                            {t('common.buttons.delete')}
                          </Button>
                        ) : null}
                        <Button
                          size='sm'
                          onClick={handleSaveRule}
                          disabled={!canWrite || saveRule.isPending}
                          data-testid='save-rule-btn'
                        >
                          {t('infrastructure.actions.saveRule')}
                        </Button>
                      </div>
                    </CardHeader>
                    <CardContent className='space-y-4'>
                      <div className='grid gap-4 sm:grid-cols-2'>
                        <div className='space-y-1.5'>
                          <Label htmlFor='rule-name'>{t('infrastructure.rules.name')}</Label>
                          <Input
                            id='rule-name'
                            value={ruleDraft.name}
                            disabled={!canWrite}
                            onChange={(e) =>
                              setRuleDraft((curr) => (curr ? { ...curr, name: e.target.value } : curr))
                            }
                          />
                        </div>
                        <div className='space-y-1.5'>
                          <Label htmlFor='rule-priority'>{t('infrastructure.rules.priority')}</Label>
                          <Input
                            id='rule-priority'
                            type='number'
                            min={0}
                            value={ruleDraft.priority}
                            disabled={!canWrite}
                            onChange={(e) =>
                              setRuleDraft((curr) =>
                                curr ? { ...curr, priority: Number(e.target.value) || 0 } : curr
                              )
                            }
                          />
                        </div>
                        <div className='space-y-1.5'>
                          <Label htmlFor='rule-path'>{t('infrastructure.rules.pathPrefix')}</Label>
                          <Input
                            id='rule-path'
                            className='font-mono'
                            value={ruleDraft.match.pathPrefix ?? '/v1/chat/completions'}
                            disabled={!canWrite}
                            onChange={(e) =>
                              setRuleDraft((curr) =>
                                curr
                                  ? {
                                      ...curr,
                                      match: { ...curr.match, pathPrefix: e.target.value },
                                    }
                                  : curr
                              )
                            }
                          />
                        </div>
                        <div className='space-y-1.5'>
                          <Label htmlFor='rule-model'>{t('infrastructure.rules.model')}</Label>
                          <Input
                            id='rule-model'
                            className='font-mono'
                            value={ruleDraft.match.model ?? ''}
                            placeholder='unowned-model-preview'
                            disabled={!canWrite}
                            onChange={(e) =>
                              setRuleDraft((curr) =>
                                curr
                                  ? {
                                      ...curr,
                                      match: { ...curr.match, model: e.target.value },
                                    }
                                  : curr
                              )
                            }
                          />
                        </div>
                        <div className='space-y-1.5 sm:col-span-2'>
                          <Label>{t('infrastructure.rules.targetNode')}</Label>
                          <Select
                            value={ruleDraft.targetNode}
                            disabled={!canWrite}
                            onValueChange={(val) =>
                              setRuleDraft((curr) => (curr ? { ...curr, targetNode: val } : curr))
                            }
                          >
                            <SelectTrigger className='font-mono'>
                              <SelectValue />
                            </SelectTrigger>
                            <SelectContent>
                              {(apiCapableNodes.length > 0 ? apiCapableNodes : nodes).map((n) => (
                                <SelectItem key={n.id} value={n.id} className='font-mono'>
                                  {n.id} ({n.provider})
                                </SelectItem>
                              ))}
                            </SelectContent>
                          </Select>
                        </div>
                      </div>

                      <div className='flex items-center justify-between rounded-lg border p-3'>
                        <Label htmlFor='rule-enabled-switch'>{t('infrastructure.nodes.enabled')}</Label>
                        <Switch
                          id='rule-enabled-switch'
                          checked={ruleDraft.enabled}
                          disabled={!canWrite}
                          onCheckedChange={(checked) =>
                            setRuleDraft((curr) => (curr ? { ...curr, enabled: checked } : curr))
                          }
                        />
                      </div>
                    </CardContent>
                  </Card>
                ) : null}
              </div>
            </TabsContent>

            {/* TAB 4: ROUTE EXPLAIN SIMULATOR */}
            <TabsContent value='explain' className='mt-0'>
              <div className='grid gap-4 lg:grid-cols-[340px_minmax(0,1fr)]'>
                <Card className='shadow-none'>
                  <CardHeader className='pb-3'>
                    <CardTitle className='text-base'>{t('infrastructure.explain.title')}</CardTitle>
                  </CardHeader>
                  <CardContent className='space-y-4'>
                    <div className='space-y-1.5'>
                      <Label>{t('infrastructure.explain.source')}</Label>
                      <Select
                        value={explainSource}
                        onValueChange={(v) => setExplainSource(v as 'draft' | 'active' | 'production')}
                      >
                        <SelectTrigger>
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value='draft'>
                            {t('infrastructure.explain.sourceDraft')}
                          </SelectItem>
                          <SelectItem value='active'>
                            {t('infrastructure.explain.sourceBaseline')}
                          </SelectItem>
                          <SelectItem value='production'>
                            {t('infrastructure.explain.sourceProduction', {
                              version: formatShortId(workerLive?.version ?? '400c7c56'),
                            })}
                          </SelectItem>
                        </SelectContent>
                      </Select>
                    </div>
                    <div className='space-y-1.5'>
                      <Label htmlFor='explain-path'>{t('infrastructure.rules.pathPrefix')}</Label>
                      <Input
                        id='explain-path'
                        className='font-mono'
                        value={explainPath}
                        onChange={(e) => setExplainPath(e.target.value)}
                      />
                    </div>
                    <div className='space-y-1.5'>
                      <Label htmlFor='explain-model'>{t('infrastructure.rules.model')}</Label>
                      <Input
                        id='explain-model'
                        className='font-mono'
                        value={explainModel}
                        onChange={(e) => setExplainModel(e.target.value)}
                      />
                    </div>
                    <Button
                      className='w-full'
                      onClick={handleRunExplain}
                      disabled={explainRoute.isPending}
                      data-testid='run-explain-btn'
                    >
                      <IconActivity className='mr-1.5 h-4 w-4' />
                      {t('infrastructure.actions.runExplain')}
                    </Button>
                  </CardContent>
                </Card>

                <Card className='shadow-none'>
                  <CardHeader className='pb-3'>
                    <CardTitle className='text-base'>
                      {t('infrastructure.explain.candidateChain')}
                    </CardTitle>
                  </CardHeader>
                  <CardContent>
                    {explainRoute.data ? (
                      <div className='space-y-3'>
                        <div className='flex flex-wrap items-center gap-3 rounded-lg border bg-muted/20 p-3.5'>
                          <div>
                            <span className='text-muted-foreground block text-xs'>
                              {t('infrastructure.explain.selectedOwner')}
                            </span>
                            <span className='font-mono text-lg font-bold' data-testid='explain-owner'>
                              {explainRoute.data.owner ?? 'key-resolver'}
                            </span>
                          </div>
                          <IconArrowRight className='text-muted-foreground h-4 w-4' />
                          <div>
                            <span className='text-muted-foreground block text-xs'>
                              {t('infrastructure.explain.decision')}
                            </span>
                            <Badge variant='default' className='font-mono'>
                              {explainRoute.data.reason}
                            </Badge>
                          </div>
                          {explainRoute.data.ruleId ? (
                            <div>
                              <span className='text-muted-foreground block text-xs'>
                                {t('infrastructure.explain.matchedRule')}
                              </span>
                              <Badge variant='outline' className='font-mono'>
                                {explainRoute.data.ruleId}
                              </Badge>
                            </div>
                          ) : null}
                        </div>

                        <div className='space-y-2'>
                          {explainRoute.data.trace.map((step, idx) => (
                            <div
                              key={`${step.code}-${idx}`}
                              className='flex items-start gap-3 rounded-md border p-2.5 text-xs'
                            >
                              <Badge variant='secondary' className='font-mono text-[11px]'>
                                {step.code}
                              </Badge>
                              <span>{step.message}</span>
                            </div>
                          ))}
                        </div>
                      </div>
                    ) : (
                      <div className='text-muted-foreground rounded-lg border border-dashed p-6 text-center text-xs'>
                        {t('infrastructure.explain.empty')}
                      </div>
                    )}
                  </CardContent>
                </Card>
              </div>
            </TabsContent>

            {/* TAB 5: RELEASES & GUARDED ACTIVATION */}
            <TabsContent value='releases' className='mt-0'>
              <div className='grid gap-4 lg:grid-cols-[340px_minmax(0,1fr)]'>
                <Card className='shadow-none'>
                  <CardHeader className='pb-3'>
                    <CardTitle className='text-base'>
                      {t('infrastructure.actions.createRelease')}
                    </CardTitle>
                    <CardDescription className='text-xs'>
                      {isRehearsal
                        ? t('infrastructure.releases.guardrailIsolated')
                        : t('infrastructure.releases.guardrailWrangler')}
                    </CardDescription>
                  </CardHeader>
                  <CardContent className='space-y-3.5'>
                    <div className='space-y-1.5'>
                      <Label htmlFor='release-note'>{t('infrastructure.releases.noteLabel')}</Label>
                      <Input
                        id='release-note'
                        value={releaseNote}
                        placeholder={t('infrastructure.releases.notePlaceholder')}
                        disabled={!canWrite}
                        onChange={(e) => setReleaseNote(e.target.value)}
                      />
                    </div>
                    <div className='flex gap-2'>
                      <Button
                        variant='outline'
                        className='flex-1'
                        disabled={!state || previewRelease.isPending}
                        data-testid='preview-release-btn'
                        onClick={() =>
                          state && previewRelease.mutate({ revision: state.revision })
                        }
                      >
                        {t('infrastructure.actions.previewStage')}
                      </Button>
                      <Button
                        className='flex-1'
                        disabled={!canWrite || !state || createRelease.isPending}
                        onClick={handleCreateRelease}
                        data-testid='create-release-btn'
                      >
                        {t('infrastructure.actions.createRelease')}
                      </Button>
                    </div>

                    {previewRelease.data ? (
                      <div className='space-y-1.5 rounded-lg border bg-muted/20 p-3 text-xs'>
                        <div className='flex items-center justify-between font-medium'>
                          <span>
                            {t('infrastructure.releases.diffTitle', {
                              base: formatShortId(previewRelease.data.baseVersion, 12),
                            })}
                          </span>
                          <Badge
                            variant={previewRelease.data.valid ? 'default' : 'destructive'}
                            className='text-[10px]'
                          >
                            {previewRelease.data.valid
                              ? t('infrastructure.status.valid')
                              : t('infrastructure.status.issues')}
                          </Badge>
                        </div>
                        {(previewRelease.data.diff ?? []).length === 0 ? (
                          <div className='text-muted-foreground'>
                            {t('infrastructure.releases.noChanges')}
                          </div>
                        ) : (
                          (previewRelease.data.diff ?? []).map((d, idx) => (
                            <div key={idx} className='font-mono text-[11px]'>
                              {d.change}: {d.section}/{d.id}
                            </div>
                          ))
                        )}
                        {(previewRelease.data.issues ?? []).map((iss, idx) => (
                          <div key={idx} className='text-destructive text-[11px]'>
                            {iss.path}: {iss.message}
                          </div>
                        ))}
                      </div>
                    ) : null}
                  </CardContent>
                </Card>

                <Card className='shadow-none'>
                  <CardHeader className='pb-3'>
                    <CardTitle className='text-base'>{t('infrastructure.releases.title')}</CardTitle>
                  </CardHeader>
                  <CardContent className='space-y-2'>
                    {(state?.versions ?? []).map((ver) => {
                      const isActive = ver.id === state?.activeVersion;
                      return (
                        <div
                          key={ver.id}
                          className='flex flex-wrap items-center justify-between gap-3 rounded-lg border p-3'
                        >
                          <div className='space-y-0.5'>
                            <div className='flex items-center gap-2'>
                              <span className='font-mono text-xs font-semibold'>
                                {formatShortId(ver.id, 16)}
                              </span>
                              {isActive ? (
                                <Badge variant='default' className='text-[10px]' data-testid={`active-badge-${ver.id}`}>
                                  <IconCheck className='mr-1 h-3 w-3' />
                                  {isRehearsal
                                    ? t('infrastructure.releases.rehearsalActiveBadge')
                                    : t('infrastructure.releases.activeBadge')}
                                </Badge>
                              ) : (
                                <Badge variant='outline' className='text-[10px]'>
                                  {ver.status === 'candidate'
                                    ? t('infrastructure.status.candidate')
                                    : ver.status === 'superseded'
                                      ? t('infrastructure.status.superseded')
                                      : ver.status}
                                </Badge>
                              )}
                              {ver.readBack?.version ? (
                                <Badge variant='secondary' className='font-mono text-[10px]'>
                                  {ver.readBack.adapter}:{formatShortId(ver.readBack.version, 8)}
                                </Badge>
                              ) : null}
                            </div>
                            <div className='text-muted-foreground text-xs'>
                              {ver.note || 'Baseline snapshot'} · rev {ver.revision}
                            </div>
                          </div>
                          {!isActive ? (
                            <Button
                              size='sm'
                              variant='outline'
                              disabled={!canWrite || activateRelease.isPending}
                              data-testid={`activate-release-${ver.id}`}
                              onClick={() => handleActivateRelease(ver.id)}
                            >
                              {ver.id === 'baseline'
                                ? isRehearsal
                                  ? t('infrastructure.actions.rollbackRehearsal')
                                  : t('infrastructure.actions.rollback')
                                : isRehearsal
                                  ? t('infrastructure.actions.activateRehearsal')
                                  : t('infrastructure.actions.activate')}
                            </Button>
                          ) : null}
                        </div>
                      );
                    })}
                  </CardContent>
                </Card>
              </div>
            </TabsContent>
          </Tabs>
        </div>
      </Main>
    </>
  );
}
