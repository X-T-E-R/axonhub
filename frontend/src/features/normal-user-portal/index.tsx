import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';
import { Check, Copy, Eye, EyeOff, RefreshCw, Search } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { useAuthStore } from '@/stores/authStore';
import { useProjectStore, useSelectedProjectId } from '@/stores/projectStore';
import { selfServiceApi, type SelfAPIKey, type SelfQuotaSummary, type SelfRequest, type SelfUsage } from '@/lib/api-client';
import { extractNumberID, normalizeEntityID } from '@/lib/utils';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { useMyProjects } from '@/features/projects/data/projects';

type RevealedSecret = {
  keyId: number;
  keyName: string;
  value: string;
};

type RequestFilters = {
  keyId: string;
  model: string;
  status: string;
  range: '24h' | '7d' | '30d' | 'all';
};

export type UserConsoleSection = 'overview' | 'models' | 'keys' | 'requests' | 'usage' | 'quickstart';

const USER_CONSOLE_SECTION_PATH: Record<
  UserConsoleSection,
  | '/self-service'
  | '/self-service/models'
  | '/self-service/api-keys'
  | '/self-service/requests'
  | '/self-service/usage'
  | '/self-service/quickstart'
> = {
  overview: '/self-service',
  models: '/self-service/models',
  keys: '/self-service/api-keys',
  requests: '/self-service/requests',
  usage: '/self-service/usage',
  quickstart: '/self-service/quickstart',
};

const MODEL_PAGE_SIZE = 12;
const PRESET_ALL = 'all';
const FILTER_ALL = 'all';

const formatNumber = (value?: number | null) => (value === null || value === undefined ? '—' : value.toLocaleString());

const formatProjectLabel = (projectID: string, index: number, projectName?: string) => {
  if (projectName?.trim()) {
    return projectName.trim();
  }
  const numericId = extractNumberID(projectID);
  return numericId ? `Project #${numericId}` : `Project ${index + 1}`;
};

const formatUsageCost = (usage?: SelfUsage) => (!usage || usage.totalCost <= 0 ? '—' : usage.totalCost.toFixed(6));

const formatQuotaSummary = (quota?: SelfQuotaSummary) => {
  if (!quota) return '';
  const parts = [
    quota.requests ? `${quota.requests.toLocaleString()} requests` : '',
    quota.totalTokens ? `${quota.totalTokens.toLocaleString()} tokens` : '',
    quota.cost ? `cost ${quota.cost}` : '',
    quota.period ? `per ${quota.period}` : '',
  ].filter(Boolean);
  return parts.join(' · ');
};

const getRangeStart = (range: RequestFilters['range']) => {
  if (range === 'all') return undefined;
  const days = range === '24h' ? 1 : range === '7d' ? 7 : 30;
  const start = new Date();
  start.setDate(start.getDate() - days);
  return start;
};

const isSelfServiceDisabledError = (error: unknown) => {
  const message = error instanceof Error ? error.message.toLowerCase() : '';
  return (
    message.includes('self-service') || message.includes('self service') || message.includes('disabled') || message.includes('forbidden')
  );
};

export default function NormalUserPortal({ initialSection = 'overview' }: { initialSection?: UserConsoleSection }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { user } = useAuthStore((state) => state.auth);
  const selectedProjectId = useSelectedProjectId();
  const setSelectedProjectId = useProjectStore((state) => state.setSelectedProjectId);
  const { data: myProjects } = useMyProjects();
  const [keyName, setKeyName] = useState('');
  const [createPresetID, setCreatePresetID] = useState('');
  const [modelPresetFilter, setModelPresetFilter] = useState(PRESET_ALL);
  const [modelSearch, setModelSearch] = useState('');
  const [visibleModelCount, setVisibleModelCount] = useState(MODEL_PAGE_SIZE);
  const [editingKeyId, setEditingKeyId] = useState<number | null>(null);
  const [editingKeyName, setEditingKeyName] = useState('');
  const [requestFilters, setRequestFilters] = useState<RequestFilters>({
    keyId: FILTER_ALL,
    model: '',
    status: FILTER_ALL,
    range: '7d',
  });
  const [revealedSecret, setRevealedSecret] = useState<RevealedSecret | null>(null);
  const [showSecret, setShowSecret] = useState(false);
  const [copiedTarget, setCopiedTarget] = useState<'base-url' | 'api-key' | 'snippet' | null>(null);

  const projectNameById = useMemo(() => new Map((myProjects ?? []).map((project) => [project.id, project.name])), [myProjects]);
  const projectOptions = useMemo(
    () =>
      (user?.projects ?? [])
        .map((project, index) => {
          const projectID = normalizeEntityID(project.projectID);
          return { id: projectID, label: formatProjectLabel(projectID, index, projectNameById.get(projectID)) };
        })
        .filter((project) => project.id),
    [projectNameById, user?.projects]
  );
  const firstProjectID = projectOptions[0]?.id ?? '';
  const selectedProjectBelongsToUser = useMemo(
    () => Boolean(selectedProjectId && projectOptions.some((project) => project.id === selectedProjectId)),
    [projectOptions, selectedProjectId]
  );
  const projectID =
    user?.isOwner && selectedProjectId
      ? selectedProjectId
      : selectedProjectBelongsToUser && selectedProjectId
        ? selectedProjectId
        : firstProjectID;
  const selectedPresetIdForApi = modelPresetFilter === PRESET_ALL ? undefined : Number(modelPresetFilter);
  const baseURL = useMemo(() => (typeof window === 'undefined' ? '/v1' : `${window.location.origin}/v1`), []);

  useEffect(() => {
    if (projectID && projectID !== selectedProjectId) {
      setSelectedProjectId(projectID);
    }
  }, [projectID, selectedProjectId, setSelectedProjectId]);

  useEffect(() => {
    setCreatePresetID('');
    setModelPresetFilter(PRESET_ALL);
    setRevealedSecret(null);
    setShowSecret(false);
    setVisibleModelCount(MODEL_PAGE_SIZE);
  }, [projectID]);

  useEffect(() => {
    setVisibleModelCount(MODEL_PAGE_SIZE);
  }, [modelPresetFilter, modelSearch]);

  const enabled = Boolean(projectID || user?.isOwner);
  const presets = useQuery({
    queryKey: ['self', 'routing-presets', projectID],
    queryFn: () => selfServiceApi.routingPresets(projectID),
    enabled,
  });
  const keys = useQuery({
    queryKey: ['self', 'api-keys', projectID],
    queryFn: () => selfServiceApi.apiKeys(projectID),
    enabled,
  });
  const models = useQuery({
    queryKey: ['self', 'models', projectID, selectedPresetIdForApi],
    queryFn: () => selfServiceApi.models(projectID, selectedPresetIdForApi),
    enabled,
  });
  const requests = useQuery({
    queryKey: ['self', 'requests', projectID],
    queryFn: () => selfServiceApi.requests(projectID, { limit: 100 }),
    enabled,
  });
  const usage = useQuery({
    queryKey: ['self', 'usage', projectID],
    queryFn: () => selfServiceApi.usage(projectID),
    enabled,
  });

  const selectedPreset = presets.data?.find((preset) => String(preset.id) === createPresetID);
  const selectedModelPreset = presets.data?.find((preset) => String(preset.id) === modelPresetFilter);
  const selectedExampleModel = models.data?.[0]?.name || models.data?.[0]?.id || 'gpt-4o-mini';
  const requestDetailsVisible = requests.data?.some((request) => request.detailsVisible) ?? false;
  const selfServiceDisabled = [presets.error, keys.error, models.error, requests.error, usage.error].some(isSelfServiceDisabledError);
  const hasNoPresets = !presets.isLoading && !presets.isError && !presets.data?.length;
  const hasNoModels = !models.isLoading && !models.isError && !models.data?.length;
  const hasNoKeys = !keys.isLoading && !keys.isError && !keys.data?.length;
  const hasNoRequests = !requests.isLoading && !requests.isError && !requests.data?.length;
  const createDisabledReason = !enabled
    ? t('selfService.empty.noProject.description')
    : selfServiceDisabled
      ? t('selfService.empty.disabled.description')
      : presets.isLoading
        ? t('selfService.keys.create.loadingPresets')
        : presets.isError
          ? t('selfService.keys.create.presetsError')
          : hasNoPresets
            ? t('selfService.empty.noPresets.description')
            : !keyName.trim()
              ? t('selfService.keys.create.nameRequired')
              : !createPresetID
                ? t('selfService.keys.create.presetRequired')
                : undefined;

  const requestStatusOptions = useMemo(
    () => Array.from(new Set(requests.data?.map((request) => request.status).filter(Boolean))).sort(),
    [requests.data]
  );
  const filteredRequests = useMemo(() => {
    const rangeStart = getRangeStart(requestFilters.range);
    const modelFilter = requestFilters.model.trim().toLowerCase();
    return (requests.data ?? []).filter((request) => {
      if (requestFilters.keyId !== FILTER_ALL && String(request.apiKeyId ?? '') !== requestFilters.keyId) return false;
      if (requestFilters.status !== FILTER_ALL && request.status !== requestFilters.status) return false;
      if (modelFilter && !request.modelId.toLowerCase().includes(modelFilter)) return false;
      if (rangeStart && new Date(request.createdAt) < rangeStart) return false;
      return true;
    });
  }, [requestFilters, requests.data]);
  const filteredModels = useMemo(() => {
    const search = modelSearch.trim().toLowerCase();
    if (!search) return models.data ?? [];
    return (models.data ?? []).filter((model) =>
      [model.id, model.name, ...(model.developers ?? []), ...(model.accessGroups?.map((group) => group.name) ?? model.groups ?? [])]
        .join(' ')
        .toLowerCase()
        .includes(search)
    );
  }, [modelSearch, models.data]);
  const visibleModels = filteredModels.slice(0, visibleModelCount);
  const canShowMoreModels = visibleModelCount < filteredModels.length;
  const firstRequestSnippet = `curl ${baseURL}/chat/completions \\
  -H "Authorization: Bearer ${revealedSecret?.value || '<YOUR_API_KEY>'}" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"${selectedExampleModel}","messages":[{"role":"user","content":"Hello"}]}'`;

  const copyText = async (value: string, target: 'base-url' | 'api-key' | 'snippet') => {
    try {
      await navigator.clipboard.writeText(value);
      setCopiedTarget(target);
      toast.success(t('selfService.toasts.copied'));
      window.setTimeout(() => setCopiedTarget((current) => (current === target ? null : current)), 1500);
    } catch {
      toast.error(t('selfService.toasts.copyFailed'));
    }
  };

  const goToSection = (section: UserConsoleSection) => {
    navigate({ to: USER_CONSOLE_SECTION_PATH[section] });
  };

  const selectPresetForKeyCreation = (presetID: number) => {
    setCreatePresetID(String(presetID));
    goToSection('keys');
  };

  const createKey = useMutation({
    mutationFn: () =>
      selfServiceApi.createAPIKey({
        projectId: projectID,
        name: keyName.trim(),
        presetId: createPresetID,
      }),
    onSuccess: async (created) => {
      await queryClient.invalidateQueries({
        queryKey: ['self', 'api-keys', projectID],
      });
      setKeyName('');
      if (created.key) {
        setRevealedSecret({
          keyId: created.id,
          keyName: created.name,
          value: created.key,
        });
        setShowSecret(false);
        toast.success(t('selfService.toasts.keyCreatedWithSecret'));
        return;
      }
      toast.success(t('selfService.toasts.keyCreated'));
    },
    onError: (error: Error) => toast.error(error.message || t('selfService.toasts.keyCreateFailed')),
  });
  const rotateKey = useMutation({
    mutationFn: async (key: SelfAPIKey) => ({
      key,
      rotated: await selfServiceApi.rotateAPIKey(key.id),
    }),
    onSuccess: async ({ key, rotated }) => {
      await queryClient.invalidateQueries({
        queryKey: ['self', 'api-keys', projectID],
      });
      if (rotated.key) {
        setRevealedSecret({
          keyId: rotated.id,
          keyName: rotated.name,
          value: rotated.key,
        });
        setShowSecret(false);
      }
      toast.success(t('selfService.toasts.keyRotated', { name: key.name }));
    },
    onError: (error: Error) => toast.error(error.message || t('selfService.toasts.keyRotateFailed')),
  });
  const updateKey = useMutation({
    mutationFn: async ({ id, name }: { id: number; name: string }) => selfServiceApi.updateAPIKey(id, { name }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: ['self', 'api-keys', projectID],
      });
      setEditingKeyId(null);
      setEditingKeyName('');
      toast.success(t('selfService.toasts.keyRenamed'));
    },
    onError: (error: Error) => toast.error(error.message || t('selfService.toasts.keyRenameFailed')),
  });
  const updateStatus = useMutation({
    mutationFn: async ({ id, status }: { id: number; status: string }) => selfServiceApi.updateAPIKeyStatus(id, status),
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: ['self', 'api-keys', projectID],
      });
      toast.success(t('selfService.toasts.keyStatusUpdated'));
    },
    onError: (error: Error) => toast.error(error.message || t('selfService.toasts.keyStatusFailed')),
  });

  const startRename = (key: SelfAPIKey) => {
    setEditingKeyId(key.id);
    setEditingKeyName(key.name);
  };
  const submitRename = (key: SelfAPIKey) => {
    const nextName = editingKeyName.trim();
    if (!nextName || nextName === key.name) {
      setEditingKeyId(null);
      setEditingKeyName('');
      return;
    }
    updateKey.mutate({ id: key.id, name: nextName });
  };

  if (!enabled) {
    return (
      <div className='mx-auto max-w-3xl p-6'>
        <EmptyState title={t('selfService.empty.noProject.title')} description={t('selfService.empty.noProject.description')} />
      </div>
    );
  }
  if (selfServiceDisabled) {
    return (
      <div className='mx-auto max-w-3xl p-6'>
        <EmptyState title={t('selfService.empty.disabled.title')} description={t('selfService.empty.disabled.description')} />
      </div>
    );
  }

  return (
    <div className='space-y-6 p-6'>
      {initialSection === 'overview' && (
        <div className='space-y-4'>
          <div className='grid gap-4 md:grid-cols-4'>
            <MetricCard value={models.isLoading ? '—' : formatNumber(models.data?.length)} label={t('selfService.metrics.models')} />
            <MetricCard value={keys.isLoading ? '—' : formatNumber(keys.data?.length)} label={t('selfService.metrics.keys')} />
            <MetricCard value={usage.isLoading ? '—' : formatNumber(usage.data?.requests)} label={t('selfService.metrics.requests')} />
            <MetricCard value={usage.isLoading ? '—' : formatNumber(usage.data?.totalTokens)} label={t('selfService.metrics.tokens')} />
          </div>

          <div className='grid gap-4 xl:grid-cols-[1fr_1fr]'>
            <Card>
              <CardHeader>
                <CardTitle>{t('selfService.overview.quickActionsTitle')}</CardTitle>
                <CardDescription>{t('selfService.overview.quickActionsDescription')}</CardDescription>
              </CardHeader>
              <CardContent className='grid gap-3 sm:grid-cols-2'>
                <ActionButton
                  title={t('selfService.overview.actions.createKey')}
                  description={t('selfService.overview.actions.createKeyHelp')}
                  onClick={() => goToSection('keys')}
                />
                <ActionButton
                  title={t('selfService.overview.actions.browseModels')}
                  description={t('selfService.overview.actions.browseModelsHelp')}
                  onClick={() => goToSection('models')}
                />
                <ActionButton
                  title={t('selfService.overview.actions.reviewRequests')}
                  description={t('selfService.overview.actions.reviewRequestsHelp')}
                  onClick={() => goToSection('requests')}
                />
                <ActionButton
                  title={t('selfService.overview.actions.copyQuickstart')}
                  description={t('selfService.overview.actions.copyQuickstartHelp')}
                  onClick={() => goToSection('quickstart')}
                />
              </CardContent>
            </Card>
            <Card>
              <CardHeader>
                <CardTitle>{t('selfService.overview.accessTitle')}</CardTitle>
                <CardDescription>{t('selfService.overview.accessDescription')}</CardDescription>
              </CardHeader>
              <CardContent className='space-y-3'>
                <div className='flex flex-wrap gap-2'>
                  {presets.isLoading && <Badge variant='secondary'>{t('selfService.overview.loadingAccessGroups')}</Badge>}
                  {!presets.isLoading &&
                    !presets.isError &&
                    !hasNoPresets &&
                    presets.data?.slice(0, 8).map((preset) => (
                      <Badge key={preset.id} variant='outline'>
                        {preset.name}
                      </Badge>
                    ))}
                  {hasNoPresets && <Badge variant='secondary'>{t('selfService.empty.noPresets.title')}</Badge>}
                  {presets.isError && <Badge variant='secondary'>{t('selfService.keys.create.presetsError')}</Badge>}
                </div>
                <StatusRow
                  ok={!hasNoModels}
                  label={t('selfService.overview.modelsStatus')}
                  detail={
                    hasNoModels
                      ? t('selfService.empty.noModels.description')
                      : t('selfService.overview.modelsReady', { count: models.data?.length ?? 0 })
                  }
                />
                <StatusRow
                  ok={!hasNoKeys}
                  label={t('selfService.overview.keysStatus')}
                  detail={
                    hasNoKeys
                      ? t('selfService.empty.noKeys.description')
                      : t('selfService.overview.keysReady', { count: keys.data?.length ?? 0 })
                  }
                />
              </CardContent>
            </Card>
          </div>

          <div className='grid gap-4 lg:grid-cols-[1.1fr_0.9fr]'>
            <FirstRequestCard
              baseURL={baseURL}
              copiedTarget={copiedTarget}
              onCopy={copyText}
              revealedSecret={revealedSecret}
              showSecret={showSecret}
              setShowSecret={setShowSecret}
              snippet={firstRequestSnippet}
            />
            <Card>
              <CardHeader>
                <CardTitle>{t('selfService.overview.recentRequestsTitle')}</CardTitle>
                <CardDescription>{t('selfService.overview.recentRequestsDescription')}</CardDescription>
              </CardHeader>
              <CardContent className='space-y-3'>
                {requests.isLoading && <p className='text-muted-foreground text-sm'>{t('selfService.requests.loading')}</p>}
                {requests.isError && <p className='text-destructive text-sm'>{t('selfService.requests.error')}</p>}
                {hasNoRequests && (
                  <EmptyState
                    title={t('selfService.empty.noRequests.title')}
                    description={t('selfService.empty.noRequests.description')}
                    compact
                  />
                )}
                {requests.data?.slice(0, 5).map((request) => (
                  <RequestRow key={request.id} request={request} />
                ))}
                {!hasNoRequests && (
                  <Button variant='outline' size='sm' onClick={() => goToSection('requests')}>
                    {t('selfService.overview.viewAllRequests')}
                  </Button>
                )}
              </CardContent>
            </Card>
          </div>
        </div>
      )}

      {initialSection === 'models' && (
        <div className='space-y-4'>
          <Card>
            <CardHeader>
              <CardTitle>{t('selfService.models.title')}</CardTitle>
              <CardDescription>
                {selectedPresetIdForApi
                  ? t('selfService.models.descriptionPreset', {
                      preset: selectedModelPreset?.name || t('selfService.models.selectedPreset'),
                    })
                  : t('selfService.models.descriptionAll')}
              </CardDescription>
            </CardHeader>
            <CardContent className='space-y-4'>
              <div className='grid gap-3 lg:grid-cols-[1fr_260px]'>
                <div className='relative'>
                  <Search className='text-muted-foreground pointer-events-none absolute top-2.5 left-3 h-4 w-4' />
                  <Input
                    aria-label={t('selfService.models.searchLabel')}
                    className='pl-9'
                    value={modelSearch}
                    onChange={(event) => setModelSearch(event.target.value)}
                    placeholder={t('selfService.models.searchPlaceholder')}
                  />
                </div>
                <Select value={modelPresetFilter} onValueChange={setModelPresetFilter}>
                  <SelectTrigger aria-label={t('selfService.models.presetFilter')} className='w-full'>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value={PRESET_ALL}>{t('selfService.models.allPresets')}</SelectItem>
                    {presets.data?.map((preset) => (
                      <SelectItem key={preset.id} value={String(preset.id)}>
                        {preset.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>

              {models.isLoading && <p className='text-muted-foreground text-sm'>{t('selfService.models.loading')}</p>}
              {models.isError && <p className='text-destructive text-sm'>{t('selfService.models.error')}</p>}
              {!models.isLoading && !models.isError && hasNoModels && (
                <EmptyState
                  title={t('selfService.empty.noModels.title')}
                  description={t('selfService.empty.noModels.description')}
                  compact
                />
              )}
              {!models.isLoading && !models.isError && !hasNoModels && !filteredModels.length && (
                <EmptyState
                  title={t('selfService.empty.noModelMatches.title')}
                  description={t('selfService.empty.noModelMatches.description')}
                  compact
                />
              )}

              <div className='grid gap-2 md:grid-cols-2 xl:grid-cols-3'>
                {visibleModels.map((model) => (
                  <div key={`${model.id}-${model.presetId ?? 'all'}`} className='space-y-2 rounded-md border p-3 text-sm'>
                    <div className='font-medium'>{model.name || model.id}</div>
                    <div className='text-muted-foreground text-xs break-all'>{model.id}</div>
                    <div className='flex flex-wrap gap-1'>
                      {((model.accessGroups?.map((group) => group.name) ?? model.groups)?.length
                        ? (model.accessGroups?.map((group) => group.name) ?? model.groups)!
                        : [t('selfService.models.defaultGroup')]
                      ).map((group) => (
                        <Badge key={`${model.id}-${group}`} variant='outline'>
                          {group}
                        </Badge>
                      ))}
                    </div>
                    {model.developers?.length ? <p className='text-muted-foreground text-xs'>{model.developers.join(', ')}</p> : null}
                    {model.presetId && (
                      <Button variant='outline' size='sm' onClick={() => selectPresetForKeyCreation(model.presetId!)}>
                        {t('selfService.models.usePreset')}
                      </Button>
                    )}
                  </div>
                ))}
              </div>

              {canShowMoreModels && (
                <div className='flex justify-center'>
                  <Button variant='outline' onClick={() => setVisibleModelCount((count) => count + MODEL_PAGE_SIZE)}>
                    {t('selfService.models.showMore', {
                      remaining: filteredModels.length - visibleModelCount,
                    })}
                  </Button>
                </div>
              )}
              {!canShowMoreModels && filteredModels.length > MODEL_PAGE_SIZE && (
                <p className='text-muted-foreground text-center text-xs'>
                  {t('selfService.models.allShown', {
                    count: filteredModels.length,
                  })}
                </p>
              )}
            </CardContent>
          </Card>
        </div>
      )}

      {initialSection === 'keys' && (
        <div className='space-y-4'>
          <div className='grid gap-4 xl:grid-cols-[1.1fr_0.9fr]'>
            <Card>
              <CardHeader>
                <CardTitle>{t('selfService.keys.create.title')}</CardTitle>
                <CardDescription>{t('selfService.keys.create.description')}</CardDescription>
              </CardHeader>
              <CardContent className='space-y-4'>
                <div className='grid gap-3 md:grid-cols-[1.1fr_0.9fr]'>
                  <div className='space-y-2'>
                    <Label htmlFor='self-key-name'>{t('selfService.keys.create.nameLabel')}</Label>
                    <Input
                      id='self-key-name'
                      value={keyName}
                      onChange={(event) => setKeyName(event.target.value)}
                      placeholder={t('selfService.keys.create.namePlaceholder')}
                    />
                  </div>
                  <div className='space-y-2'>
                    <Label htmlFor='self-preset'>{t('selfService.keys.create.presetLabel')}</Label>
                    <Select value={createPresetID} onValueChange={setCreatePresetID}>
                      <SelectTrigger id='self-preset' className='w-full' aria-label={t('selfService.keys.create.presetLabel')}>
                        <SelectValue placeholder={t('selfService.keys.create.presetPlaceholder')} />
                      </SelectTrigger>
                      <SelectContent>
                        {presets.data?.map((preset) => (
                          <SelectItem key={preset.id} value={String(preset.id)}>
                            {preset.name}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                </div>

                {selectedPreset && (
                  <div className='rounded-md border border-dashed p-3 text-sm'>
                    <div className='font-medium'>{selectedPreset.name}</div>
                    <p className='text-muted-foreground mt-1 text-xs'>
                      {selectedPreset.description || t('selfService.keys.create.presetFallback')}
                    </p>
                    {selectedPreset.profileLabel && (
                      <Badge variant='outline' className='mt-2'>
                        {selectedPreset.profileLabel}
                      </Badge>
                    )}
                    {selectedPreset.modelCount !== undefined && (
                      <Badge variant='secondary' className='mt-2'>
                        {t('selfService.keys.create.modelCount', {
                          count: selectedPreset.modelCount,
                        })}
                      </Badge>
                    )}
                    {formatQuotaSummary(selectedPreset.quotaSummary) && (
                      <p className='text-muted-foreground mt-2 text-xs'>{formatQuotaSummary(selectedPreset.quotaSummary)}</p>
                    )}
                  </div>
                )}

                <div className='flex flex-wrap items-center gap-2'>
                  <Button disabled={Boolean(createDisabledReason) || createKey.isPending} onClick={() => createKey.mutate()}>
                    {createKey.isPending ? t('selfService.keys.create.creating') : t('selfService.keys.create.submit')}
                  </Button>
                  {createDisabledReason && <p className='text-muted-foreground text-xs'>{createDisabledReason}</p>}
                </div>
              </CardContent>
            </Card>
            <FirstRequestCard
              baseURL={baseURL}
              copiedTarget={copiedTarget}
              onCopy={copyText}
              revealedSecret={revealedSecret}
              showSecret={showSecret}
              setShowSecret={setShowSecret}
              snippet={firstRequestSnippet}
            />
          </div>

          <Card>
            <CardHeader>
              <CardTitle>{t('selfService.keys.list.title')}</CardTitle>
              <CardDescription>{t('selfService.keys.list.description')}</CardDescription>
            </CardHeader>
            <CardContent className='space-y-3'>
              {keys.isLoading && <p className='text-muted-foreground text-sm'>{t('selfService.keys.list.loading')}</p>}
              {keys.isError && <p className='text-destructive text-sm'>{t('selfService.keys.list.error')}</p>}
              {hasNoKeys && (
                <EmptyState title={t('selfService.empty.noKeys.title')} description={t('selfService.empty.noKeys.description')} compact />
              )}
              {keys.data?.map((key) => {
                const isStatusUpdating = updateStatus.isPending && updateStatus.variables?.id === key.id;
                const isRotating = rotateKey.isPending && rotateKey.variables?.id === key.id;
                const isRenaming = updateKey.isPending && updateKey.variables?.id === key.id;
                const nextStatus = key.status === 'enabled' ? 'disabled' : 'enabled';
                const isEditing = editingKeyId === key.id;
                return (
                  <div key={key.id} className='space-y-3 rounded-md border p-3 text-sm'>
                    <div className='flex flex-wrap items-start justify-between gap-2'>
                      <div className='min-w-0 flex-1'>
                        {isEditing ? (
                          <div className='flex max-w-xl flex-wrap gap-2'>
                            <Input
                              aria-label={t('selfService.keys.rename.inputLabel')}
                              value={editingKeyName}
                              onChange={(event) => setEditingKeyName(event.target.value)}
                              onKeyDown={(event) => {
                                if (event.key === 'Enter') submitRename(key);
                                if (event.key === 'Escape') {
                                  setEditingKeyId(null);
                                  setEditingKeyName('');
                                }
                              }}
                            />
                            <Button size='sm' disabled={isRenaming} onClick={() => submitRename(key)}>
                              {t('common.buttons.save')}
                            </Button>
                            <Button
                              size='sm'
                              variant='outline'
                              onClick={() => {
                                setEditingKeyId(null);
                                setEditingKeyName('');
                              }}
                            >
                              {t('common.buttons.cancel')}
                            </Button>
                          </div>
                        ) : (
                          <div className='font-medium'>{key.name}</div>
                        )}
                        <div className='text-muted-foreground mt-1 text-xs'>
                          {(key.activeProfile || t('selfService.keys.list.noPreset')) +
                            ` · ${t('selfService.keys.list.updated', { time: new Date(key.updatedAt).toLocaleString() })}`}
                        </div>
                      </div>
                      <Badge variant={key.status === 'enabled' ? 'default' : 'secondary'}>{key.status}</Badge>
                    </div>
                    <div className='flex flex-wrap gap-2'>
                      <Button variant='outline' size='sm' disabled={isEditing} onClick={() => startRename(key)}>
                        {t('selfService.keys.actions.rename')}
                      </Button>
                      <Button variant='outline' size='sm' disabled={isRotating} onClick={() => rotateKey.mutate(key)}>
                        <RefreshCw className='h-4 w-4' />
                        {t('selfService.keys.actions.rotate')}
                      </Button>
                      <Button
                        variant='outline'
                        size='sm'
                        disabled={isStatusUpdating}
                        onClick={() =>
                          updateStatus.mutate({
                            id: key.id,
                            status: nextStatus,
                          })
                        }
                      >
                        {key.status === 'enabled' ? t('selfService.keys.actions.disable') : t('selfService.keys.actions.enable')}
                      </Button>
                      <Button
                        variant='outline'
                        size='sm'
                        disabled={isStatusUpdating || key.status === 'archived'}
                        onClick={() =>
                          updateStatus.mutate({
                            id: key.id,
                            status: 'archived',
                          })
                        }
                      >
                        {t('selfService.keys.actions.archive')}
                      </Button>
                    </div>
                    {revealedSecret?.keyId === key.id && (
                      <div className='text-muted-foreground rounded-md border border-dashed p-3 text-xs'>
                        {t('selfService.keys.list.secretReady')}
                      </div>
                    )}
                  </div>
                );
              })}
            </CardContent>
          </Card>
        </div>
      )}

      {initialSection === 'requests' && (
        <div className='space-y-4'>
          <Card>
            <CardHeader>
              <CardTitle>{t('selfService.requests.title')}</CardTitle>
              <CardDescription>
                {requestDetailsVisible ? t('selfService.requests.detailsEnabled') : t('selfService.requests.detailsHidden')}
              </CardDescription>
            </CardHeader>
            <CardContent className='space-y-4'>
              <div className='grid gap-3 lg:grid-cols-4'>
                <Select
                  value={requestFilters.range}
                  onValueChange={(range: RequestFilters['range']) => setRequestFilters((prev) => ({ ...prev, range }))}
                >
                  <SelectTrigger aria-label={t('selfService.requests.filters.range')} className='w-full'>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value='24h'>{t('selfService.requests.filters.last24h')}</SelectItem>
                    <SelectItem value='7d'>{t('selfService.requests.filters.last7d')}</SelectItem>
                    <SelectItem value='30d'>{t('selfService.requests.filters.last30d')}</SelectItem>
                    <SelectItem value='all'>{t('selfService.requests.filters.allTime')}</SelectItem>
                  </SelectContent>
                </Select>
                <Select value={requestFilters.keyId} onValueChange={(keyId) => setRequestFilters((prev) => ({ ...prev, keyId }))}>
                  <SelectTrigger aria-label={t('selfService.requests.filters.key')} className='w-full'>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value={FILTER_ALL}>{t('selfService.requests.filters.allKeys')}</SelectItem>
                    {keys.data?.map((key) => (
                      <SelectItem key={key.id} value={String(key.id)}>
                        {key.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <Select value={requestFilters.status} onValueChange={(status) => setRequestFilters((prev) => ({ ...prev, status }))}>
                  <SelectTrigger aria-label={t('selfService.requests.filters.status')} className='w-full'>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value={FILTER_ALL}>{t('selfService.requests.filters.allStatuses')}</SelectItem>
                    {requestStatusOptions.map((status) => (
                      <SelectItem key={status} value={status}>
                        {status}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <Input
                  aria-label={t('selfService.requests.filters.model')}
                  value={requestFilters.model}
                  onChange={(event) =>
                    setRequestFilters((prev) => ({
                      ...prev,
                      model: event.target.value,
                    }))
                  }
                  placeholder={t('selfService.requests.filters.modelPlaceholder')}
                />
              </div>

              {requests.isLoading && <p className='text-muted-foreground text-sm'>{t('selfService.requests.loading')}</p>}
              {requests.isError && <p className='text-destructive text-sm'>{t('selfService.requests.error')}</p>}
              {hasNoRequests && (
                <EmptyState
                  title={t('selfService.empty.noRequests.title')}
                  description={t('selfService.empty.noRequests.description')}
                  compact
                />
              )}
              {!hasNoRequests && !filteredRequests.length && (
                <EmptyState
                  title={t('selfService.empty.noRequestMatches.title')}
                  description={t('selfService.empty.noRequestMatches.description')}
                  compact
                />
              )}
              <div className='space-y-2'>
                {filteredRequests.map((request) => (
                  <RequestRow key={request.id} request={request} />
                ))}
              </div>
            </CardContent>
          </Card>
        </div>
      )}

      {initialSection === 'usage' && (
        <div className='space-y-4'>
          <Card>
            <CardHeader>
              <CardTitle>{t('selfService.usage.title')}</CardTitle>
              <CardDescription>{t('selfService.usage.description')}</CardDescription>
            </CardHeader>
            <CardContent className='space-y-4'>
              <div className='grid gap-3 md:grid-cols-4'>
                <MetricBox label={t('selfService.usage.requests')} value={usage.isLoading ? '—' : formatNumber(usage.data?.requests)} />
                <MetricBox
                  label={t('selfService.usage.promptTokens')}
                  value={usage.isLoading ? '—' : formatNumber(usage.data?.promptTokens)}
                />
                <MetricBox
                  label={t('selfService.usage.completionTokens')}
                  value={usage.isLoading ? '—' : formatNumber(usage.data?.completionTokens)}
                />
                <MetricBox label={t('selfService.usage.operationalCost')} value={usage.isLoading ? '—' : formatUsageCost(usage.data)} />
              </div>
              <div className='text-muted-foreground rounded-md border border-dashed p-3 text-sm'>{t('selfService.usage.filterNote')}</div>
            </CardContent>
          </Card>
        </div>
      )}

      {initialSection === 'quickstart' && (
        <div className='space-y-4'>
          <FirstRequestCard
            baseURL={baseURL}
            copiedTarget={copiedTarget}
            onCopy={copyText}
            revealedSecret={revealedSecret}
            showSecret={showSecret}
            setShowSecret={setShowSecret}
            snippet={firstRequestSnippet}
          />
          <Card>
            <CardHeader>
              <CardTitle>{t('selfService.quickstart.stepsTitle')}</CardTitle>
              <CardDescription>{t('selfService.quickstart.stepsDescription')}</CardDescription>
            </CardHeader>
            <CardContent className='grid gap-3 md:grid-cols-3'>
              <QuickstartStep
                index={1}
                title={t('selfService.quickstart.stepModels')}
                description={t('selfService.quickstart.stepModelsHelp')}
              />
              <QuickstartStep index={2} title={t('selfService.quickstart.stepKey')} description={t('selfService.quickstart.stepKeyHelp')} />
              <QuickstartStep
                index={3}
                title={t('selfService.quickstart.stepRequest')}
                description={t('selfService.quickstart.stepRequestHelp')}
              />
            </CardContent>
          </Card>
        </div>
      )}
    </div>
  );
}

function MetricCard({ value, label }: { value: string; label: string }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{value}</CardTitle>
        <CardDescription>{label}</CardDescription>
      </CardHeader>
    </Card>
  );
}

function ActionButton({ title, description, onClick }: { title: string; description: string; onClick: () => void }) {
  return (
    <button type='button' onClick={onClick} className='hover:bg-muted/60 rounded-md border p-4 text-left transition-colors'>
      <div className='text-sm font-medium'>{title}</div>
      <p className='text-muted-foreground mt-1 text-xs'>{description}</p>
    </button>
  );
}

function MetricBox({ label, value }: { label: string; value: string }) {
  return (
    <div className='rounded-md border p-3'>
      <div className='text-muted-foreground text-xs'>{label}</div>
      <div className='text-lg font-semibold'>{value}</div>
    </div>
  );
}

function EmptyState({ title, description, compact = false }: { title: string; description: string; compact?: boolean }) {
  return (
    <Card className={compact ? 'border-dashed' : undefined}>
      <CardHeader>
        <CardTitle className={compact ? 'text-base' : undefined}>{title}</CardTitle>
        <CardDescription>{description}</CardDescription>
      </CardHeader>
    </Card>
  );
}

function StatusRow({ ok, label, detail }: { ok: boolean; label: string; detail: string }) {
  const { t } = useTranslation();
  return (
    <div className='flex items-start justify-between gap-3 rounded-md border p-3'>
      <div>
        <div className='font-medium'>{label}</div>
        <div className='text-muted-foreground text-xs'>{detail}</div>
      </div>
      <Badge variant={ok ? 'default' : 'secondary'}>{ok ? t('selfService.overview.ok') : t('selfService.overview.action')}</Badge>
    </div>
  );
}

function QuickstartStep({ index, title, description }: { index: number; title: string; description: string }) {
  return (
    <div className='rounded-md border p-4 text-sm'>
      <div className='bg-primary text-primary-foreground mb-3 flex h-8 w-8 items-center justify-center rounded-full text-sm font-semibold'>
        {index}
      </div>
      <div className='font-medium'>{title}</div>
      <p className='text-muted-foreground mt-1 text-xs'>{description}</p>
    </div>
  );
}

function FirstRequestCard({
  baseURL,
  copiedTarget,
  onCopy,
  revealedSecret,
  showSecret,
  setShowSecret,
  snippet,
}: {
  baseURL: string;
  copiedTarget: 'base-url' | 'api-key' | 'snippet' | null;
  onCopy: (value: string, target: 'base-url' | 'api-key' | 'snippet') => void;
  revealedSecret: RevealedSecret | null;
  showSecret: boolean;
  setShowSecret: (value: boolean | ((current: boolean) => boolean)) => void;
  snippet: string;
}) {
  const { t } = useTranslation();
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('selfService.firstRequest.title')}</CardTitle>
        <CardDescription>{t('selfService.firstRequest.description')}</CardDescription>
      </CardHeader>
      <CardContent className='space-y-4'>
        <div className='flex flex-wrap items-center justify-between gap-2 rounded-md border p-3 text-xs'>
          <div>
            <div className='text-muted-foreground'>{t('selfService.firstRequest.baseUrl')}</div>
            <code className='text-sm'>{baseURL}</code>
          </div>
          <Button variant='outline' size='sm' onClick={() => onCopy(baseURL, 'base-url')}>
            {copiedTarget === 'base-url' ? <Check className='h-4 w-4' /> : <Copy className='h-4 w-4' />}
            {t('common.buttons.copy')}
          </Button>
        </div>
        {revealedSecret ? (
          <div className='space-y-3 rounded-md border border-dashed p-3'>
            <div className='flex flex-wrap items-center justify-between gap-2'>
              <div>
                <p className='font-medium'>{revealedSecret.keyName}</p>
                <p className='text-muted-foreground text-xs'>{t('selfService.firstRequest.secretHelp')}</p>
              </div>
              <div className='flex gap-2'>
                <Button variant='outline' size='sm' onClick={() => setShowSecret((current) => !current)}>
                  {showSecret ? <EyeOff className='h-4 w-4' /> : <Eye className='h-4 w-4' />}
                  {showSecret ? t('selfService.firstRequest.hide') : t('selfService.firstRequest.reveal')}
                </Button>
                <Button variant='outline' size='sm' onClick={() => onCopy(revealedSecret.value, 'api-key')}>
                  {copiedTarget === 'api-key' ? <Check className='h-4 w-4' /> : <Copy className='h-4 w-4' />}
                  {t('selfService.firstRequest.copyKey')}
                </Button>
              </div>
            </div>
            <code className='bg-muted block rounded px-3 py-2 text-xs break-all'>
              {showSecret ? revealedSecret.value : '•'.repeat(Math.max(revealedSecret.value.length, 16))}
            </code>
          </div>
        ) : (
          <div className='text-muted-foreground rounded-md border border-dashed p-3 text-sm'>{t('selfService.firstRequest.noSecret')}</div>
        )}
        <div className='space-y-2'>
          <div className='flex items-center justify-between gap-2'>
            <Label>{t('selfService.firstRequest.curlExample')}</Label>
            <Button variant='outline' size='sm' onClick={() => onCopy(snippet, 'snippet')}>
              {copiedTarget === 'snippet' ? <Check className='h-4 w-4' /> : <Copy className='h-4 w-4' />}
              {t('common.buttons.copy')}
            </Button>
          </div>
          <pre className='bg-muted overflow-x-auto rounded-md p-3 text-xs'>
            <code>{snippet}</code>
          </pre>
        </div>
      </CardContent>
    </Card>
  );
}

function RequestRow({ request }: { request: SelfRequest }) {
  const { t } = useTranslation();
  return (
    <div className='rounded-md border p-3 text-sm'>
      <div className='flex flex-wrap items-center justify-between gap-2'>
        <span className='font-medium'>{request.modelId}</span>
        <div className='flex gap-2'>
          <Badge variant='secondary'>{request.status}</Badge>
          <Badge variant='outline'>
            {request.detailsVisible ? t('selfService.requests.badges.detailsEnabled') : t('selfService.requests.badges.metadataOnly')}
          </Badge>
        </div>
      </div>
      <div className='text-muted-foreground mt-1 text-xs'>
        {new Date(request.createdAt).toLocaleString()} · {request.source} ·{' '}
        {request.stream ? t('selfService.requests.streaming') : t('selfService.requests.nonStreaming')}
        {request.latencyMs ? ` · ${request.latencyMs} ms` : ''}
      </div>
      {!request.detailsVisible && (
        <div className='text-muted-foreground mt-2 rounded border border-dashed p-2 text-xs'>
          {t('selfService.requests.metadataOnlyHelp')}
        </div>
      )}
    </div>
  );
}
