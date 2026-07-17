import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Loader2 } from 'lucide-react';
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { useSelectedProjectId } from '@/stores/projectStore';
import { accessGroupsApi, type AdminAccessGroup, type AdminAccessGroupInput, type SelfAccessGroupProfile } from '@/lib/api-client';
import { extractNumberID } from '@/lib/utils';
import { usePermissions } from '@/hooks/usePermissions';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Checkbox } from '@/components/ui/checkbox';
import { Input } from '@/components/ui/input';
import { Textarea } from '@/components/ui/textarea';
import { useAllChannelSummarys } from '@/features/channels/data/channels';
import { useMyProjects } from '@/features/projects/data/projects';
import { formatPolicyQuota } from '@/features/apikeys/components/classification-preview';

function describeQuota(profile: SelfAccessGroupProfile | undefined, t: TFunction) {
  const quota = profile?.quotaSummary;
  if (!quota) return '';
  return formatPolicyQuota(quota, t);
}

function primaryProfile(group: AdminAccessGroup) {
  return group.profiles[0];
}

function parseModelIDs(value: string) {
  return Array.from(
    new Set(
      value
        .split(/[\n,]+/)
        .map((item) => item.trim())
        .filter(Boolean)
    )
  ).sort((a, b) => a.localeCompare(b));
}

function formatModelIDs(modelIDs?: string[]) {
  return (modelIDs ?? []).join('\n');
}

export default function AccessGroups() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const { apiKeyPermissions, channelPermissions } = usePermissions();
  const canReadPolicy = apiKeyPermissions.canRead && channelPermissions.canRead;
  const canEditPolicy = canReadPolicy && apiKeyPermissions.canWrite;
  const selectedProjectId = useSelectedProjectId();
  const [selectedGroupId, setSelectedGroupId] = useState<number | null>(null);
  const [selectedChannelIds, setSelectedChannelIds] = useState<string[]>([]);
  const [isCreatingGroup, setIsCreatingGroup] = useState(false);
  const [groupName, setGroupName] = useState('');
  const [groupDescription, setGroupDescription] = useState('');
  const [modelIDsText, setModelIDsText] = useState('');
  const { data: myProjects } = useMyProjects();
  const projectId = selectedProjectId || null;
  const projectName = myProjects?.find((project) => project.id === projectId)?.name;

  const {
    data: accessGroups,
    isLoading: isLoadingGroups,
    isError: accessGroupsError,
    error: accessGroupsLoadError,
  } = useQuery({
    queryKey: ['adminAccessGroups', projectId],
    queryFn: async () => accessGroupsApi.list(projectId!),
    enabled: Boolean(projectId) && canReadPolicy,
  });
  const {
    data: channelSummarys,
    isLoading: isLoadingChannels,
    isError: channelsError,
  } = useAllChannelSummarys(projectId, {
    enabled: Boolean(projectId) && canReadPolicy,
    includeArchived: false,
  });

  const selectedGroup = useMemo(
    () => (accessGroups ?? []).find((group) => group.id === selectedGroupId) ?? accessGroups?.[0],
    [accessGroups, selectedGroupId]
  );
  const channels = useMemo(() => channelSummarys?.edges.map((edge) => edge.node) ?? [], [channelSummarys]);
  const availableModelIDs = useMemo(() => {
    const sourceChannels = selectedChannelIds.length ? channels.filter((channel) => selectedChannelIds.includes(channel.id)) : channels;
    return Array.from(
      new Set(
        sourceChannels.flatMap((channel) =>
          (channel.allModelEntries ?? [])
            .flatMap((entry) => [entry.requestModel, entry.actualModel])
            .filter((model): model is string => Boolean(model))
        )
      )
    ).sort((a, b) => a.localeCompare(b));
  }, [channels, selectedChannelIds]);

  const saveGroupDetails = useMutation({
    mutationFn: async () => {
      if (!canEditPolicy) throw new Error(t('accessGroups.errors.policyPermission'));
      const name = groupName.trim();
      if (!name) {
        throw new Error(t('accessGroups.details.nameRequired'));
      }
      const input: AdminAccessGroupInput = {
        name,
        description: groupDescription.trim(),
        modelIds: parseModelIDs(modelIDsText),
      };
      if (isCreatingGroup) {
        input.projectId = projectId!;
        return accessGroupsApi.create(input);
      }
      if (!selectedGroup) {
        throw new Error(t('accessGroups.errors.groupMissing'));
      }
      return accessGroupsApi.update(selectedGroup.id, input);
    },
    onSuccess: async (group) => {
      setIsCreatingGroup(false);
      setSelectedGroupId(group.id);
      await refreshAccessGroups();
      toast.success(isCreatingGroup ? t('accessGroups.toasts.created') : t('accessGroups.toasts.detailsUpdated'));
    },
    onError: (error: Error) =>
      toast.error(
        error.message || (isCreatingGroup ? t('accessGroups.toasts.createFailed') : t('accessGroups.toasts.detailsUpdateFailed'))
      ),
  });

  const updateGroupVisibility = useMutation({
    mutationFn: async ({ group, visible }: { group: AdminAccessGroup; visible: boolean }) => {
      if (!canEditPolicy) throw new Error(t('accessGroups.errors.policyPermission'));
      return accessGroupsApi.update(group.id, { selfServiceVisible: visible });
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['adminAccessGroups', projectId] });
      toast.success(t('accessGroups.toasts.updated'));
    },
    onError: (error: Error) => toast.error(error.message || t('accessGroups.toasts.updateFailed')),
  });

  const assignChannels = useMutation({
    mutationFn: async () => {
      if (!canEditPolicy) throw new Error(t('accessGroups.errors.policyPermission'));
      if (!selectedGroup) {
        throw new Error(t('accessGroups.errors.groupMissing'));
      }
      return accessGroupsApi.assignChannels(selectedGroup.id, selectedChannelIds);
    },
    onSuccess: async (group) => {
      setSelectedGroupId(group.id);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['adminAccessGroups', projectId] }),
        queryClient.invalidateQueries({ queryKey: ['allChannelSummarys', projectId] }),
      ]);
      toast.success(t('accessGroups.toasts.channelsUpdated'));
    },
    onError: (error: Error) => toast.error(error.message || t('accessGroups.toasts.channelsUpdateFailed')),
  });

  const refreshAccessGroups = async () => {
    await queryClient.invalidateQueries({ queryKey: ['adminAccessGroups', projectId] });
  };

  useEffect(() => {
    setSelectedGroupId(null);
    setSelectedChannelIds([]);
    setIsCreatingGroup(false);
    setGroupName('');
    setGroupDescription('');
    setModelIDsText('');
  }, [projectId]);

  useEffect(() => {
    if (canReadPolicy) return;
    setSelectedGroupId(null);
    setSelectedChannelIds([]);
    setIsCreatingGroup(false);
    setGroupName('');
    setGroupDescription('');
    setModelIDsText('');
  }, [canReadPolicy]);

  useEffect(() => {
    if (!selectedGroupId && accessGroups?.length) {
      setSelectedGroupId(accessGroups[0].id);
    }
  }, [accessGroups, selectedGroupId]);

  useEffect(() => {
    if (!selectedGroup) {
      setSelectedChannelIds([]);
      return;
    }
    const explicitIDs = new Set((selectedGroup.channelAssignment.channelIds ?? []).map(String));
    const assignmentTags = selectedGroup.channelAssignment.tags ?? [];
    const inferredIDs = channels
      .filter((channel) => {
        const numericID = extractNumberID(channel.id);
        return (numericID && explicitIDs.has(String(numericID))) || assignmentTags.some((tag) => (channel.tags ?? []).includes(tag));
      })
      .map((channel) => channel.id);
    setSelectedChannelIds(inferredIDs);
  }, [channels, selectedGroup]);

  useEffect(() => {
    if (isCreatingGroup) return;
    setGroupName(selectedGroup?.name ?? '');
    setGroupDescription(selectedGroup?.description ?? '');
    setModelIDsText(formatModelIDs(selectedGroup ? primaryProfile(selectedGroup)?.modelIds : []));
  }, [isCreatingGroup, selectedGroup]);

  const setGroupVisible = (group: AdminAccessGroup, visible: boolean) => {
    updateGroupVisibility.mutate({ group, visible });
  };

  const startCreateGroup = () => {
    if (!canEditPolicy) return;
    setIsCreatingGroup(true);
    setGroupName('');
    setGroupDescription('');
    setModelIDsText('');
  };
  const submitGroupDetails = () => {
    saveGroupDetails.mutate();
  };
  const toggleChannel = (channelId: string, checked: boolean) => {
    setSelectedChannelIds((current) => (checked ? Array.from(new Set([...current, channelId])) : current.filter((id) => id !== channelId)));
  };
  const toggleModel = (modelId: string, checked: boolean) => {
    const current = parseModelIDs(modelIDsText);
    const next = checked ? Array.from(new Set([...current, modelId])) : current.filter((id) => id !== modelId);
    setModelIDsText(formatModelIDs(next));
  };

  if (!projectId) {
    return (
      <div className='mx-auto max-w-3xl p-6'>
        <Card>
          <CardHeader>
            <CardTitle>{t('accessGroups.noProject.title')}</CardTitle>
            <CardDescription>{t('accessGroups.noProject.description')}</CardDescription>
          </CardHeader>
        </Card>
      </div>
    );
  }

  const isLoading = isLoadingGroups;
  const isError = accessGroupsError;

  return (
    <div className='space-y-6 p-6'>
      <div className='flex flex-wrap items-start justify-between gap-4'>
        <div className='space-y-2'>
          <h1 className='text-2xl font-semibold tracking-tight'>{t('accessGroups.title')}</h1>
          <p className='text-muted-foreground max-w-3xl text-sm'>{t('accessGroups.description')}</p>
          <div className='flex flex-wrap gap-2'>
            <Badge variant='outline'>{projectName || t('accessGroups.badges.currentProject')}</Badge>
          </div>
        </div>
        <div className='flex flex-wrap gap-2'>
          {canEditPolicy && (
            <Button variant='outline' onClick={startCreateGroup}>
              {t('accessGroups.actions.create')}
            </Button>
          )}
        </div>
      </div>

      {!canReadPolicy && (
        <div role='note' className='border-amber-500/40 bg-amber-500/5 rounded-md border p-4 text-sm'>
          <div className='font-medium'>{t('accessGroups.permissions.readOnlyTitle')}</div>
          <p className='text-muted-foreground mt-1'>{t('accessGroups.permissions.requiresPolicyRead')}</p>
        </div>
      )}

      {canReadPolicy && isLoading && (
        <div className='flex h-32 items-center justify-center'>
          <Loader2 className='h-6 w-6 animate-spin' />
          <span className='text-muted-foreground ml-2'>{t('common.loading')}</span>
        </div>
      )}

      {canReadPolicy && isError && (
        <Card>
          <CardHeader>
            <CardTitle>{t('accessGroups.errors.loadTitle')}</CardTitle>
            <CardDescription>
              {accessGroupsLoadError instanceof Error ? accessGroupsLoadError.message : t('accessGroups.errors.loadDescription')}
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {canReadPolicy && !isLoading && !isError && !accessGroups?.length && (
        <Card>
          <CardHeader>
            <CardTitle>{t('accessGroups.empty.title')}</CardTitle>
            <CardDescription>{t('accessGroups.empty.description')}</CardDescription>
          </CardHeader>
          {canEditPolicy && (
            <CardContent>
              <Button onClick={startCreateGroup}>{t('accessGroups.actions.create')}</Button>
            </CardContent>
          )}
        </Card>
      )}

      {canReadPolicy && canEditPolicy && (isCreatingGroup || selectedGroup) && (
        <Card>
          <CardHeader>
            <CardTitle>{isCreatingGroup ? t('accessGroups.details.createTitle') : t('accessGroups.details.editTitle')}</CardTitle>
            <CardDescription>{t('accessGroups.details.description')}</CardDescription>
          </CardHeader>
          <CardContent className='space-y-4'>
            <div className='grid gap-3 md:grid-cols-[minmax(0,320px)_1fr_auto] md:items-end'>
              <div className='space-y-2'>
                <label className='text-sm font-medium' htmlFor='access-group-name'>
                  {t('accessGroups.details.name')}
                </label>
                <Input id='access-group-name' value={groupName} onChange={(event) => setGroupName(event.target.value)} />
              </div>
              <div className='space-y-2'>
                <label className='text-sm font-medium' htmlFor='access-group-description'>
                  {t('accessGroups.details.descriptionLabel')}
                </label>
                <Textarea
                  id='access-group-description'
                  value={groupDescription}
                  onChange={(event) => setGroupDescription(event.target.value)}
                  rows={2}
                />
              </div>
              <div className='flex flex-wrap gap-2'>
                <Button disabled={saveGroupDetails.isPending} onClick={submitGroupDetails}>
                  {saveGroupDetails.isPending ? t('common.buttons.saving') : t('common.buttons.save')}
                </Button>
                {isCreatingGroup && (
                  <Button
                    variant='outline'
                    onClick={() => {
                      setIsCreatingGroup(false);
                      setGroupName(selectedGroup?.name ?? '');
                      setGroupDescription(selectedGroup?.description ?? '');
                      setModelIDsText(formatModelIDs(selectedGroup ? primaryProfile(selectedGroup)?.modelIds : []));
                    }}
                  >
                    {t('common.buttons.cancel')}
                  </Button>
                )}
              </div>
            </div>
            <div className='grid gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(320px,0.85fr)]'>
              <div className='space-y-2'>
                <label className='text-sm font-medium' htmlFor='access-group-models'>
                  {t('accessGroups.models.allowedLabel')}
                </label>
                <Textarea
                  id='access-group-models'
                  value={modelIDsText}
                  onChange={(event) => setModelIDsText(event.target.value)}
                  placeholder={t('accessGroups.models.placeholder')}
                  rows={6}
                />
                <p className='text-muted-foreground text-xs'>{t('accessGroups.models.help')}</p>
              </div>
              <div className='space-y-2'>
                <div className='text-sm font-medium'>{t('accessGroups.models.availableTitle')}</div>
                <div className='max-h-44 space-y-2 overflow-y-auto rounded-md border p-2'>
                  {availableModelIDs.length === 0 && (
                    <p className='text-muted-foreground p-2 text-xs'>{t('accessGroups.models.noAvailable')}</p>
                  )}
                  {availableModelIDs.map((modelID) => {
                    const checked = parseModelIDs(modelIDsText).includes(modelID);
                    return (
                      <label key={modelID} className='hover:bg-muted/60 flex cursor-pointer items-center gap-2 rounded px-2 py-1 text-xs'>
                        <Checkbox checked={checked} onCheckedChange={(value) => toggleModel(modelID, Boolean(value))} />
                        <span className='break-all'>{modelID}</span>
                      </label>
                    );
                  })}
                </div>
              </div>
            </div>
          </CardContent>
        </Card>
      )}

      {canReadPolicy && <div className='grid gap-4 xl:grid-cols-[minmax(0,1fr)_minmax(360px,0.85fr)]'>
        <div className='grid gap-4 lg:grid-cols-2 xl:grid-cols-1'>
          {accessGroups?.map((group) => {
            const visible = group.selfServiceVisible;
            const profile = primaryProfile(group);
            const modelCount = profile?.modelCount ?? 0;
            const tagCount = group.channelAssignment.tags?.length ?? 0;
            const quotaSummary = describeQuota(profile, t);
            const modelPreview = profile?.modelPreview ?? [];

            return (
              <Card key={group.id} className={visible ? 'border-primary/40' : undefined}>
                <CardHeader>
                  <div className='flex flex-wrap items-start justify-between gap-3'>
                    <div className='space-y-1'>
                      <CardTitle>{group.name}</CardTitle>
                      <CardDescription>{group.description || t('accessGroups.card.noDescription')}</CardDescription>
                    </div>
                    <Badge variant={visible ? 'default' : 'secondary'}>
                      {visible ? t('accessGroups.card.visible') : t('accessGroups.card.hidden')}
                    </Badge>
                  </div>
                </CardHeader>
                <CardContent className='space-y-4'>
                  {channelPermissions.canRead && (
                    <>
                      <div className='grid gap-2 text-sm sm:grid-cols-3'>
                        <SummaryBox
                          label={t('accessGroups.card.models')}
                          value={modelCount ? String(modelCount) : t('accessGroups.card.anyModel')}
                        />
                        <SummaryBox
                          label={t('accessGroups.card.channels')}
                          value={
                            group.channelAssignment.channelCount
                              ? String(group.channelAssignment.channelCount)
                              : group.channelAssignment.tags?.length
                                ? t('accessGroups.card.byTags')
                                : t('accessGroups.card.noChannels')
                          }
                        />
                        <SummaryBox
                          label={t('accessGroups.card.tags')}
                          value={tagCount ? String(tagCount) : t('accessGroups.card.noTags')}
                        />
                      </div>

                      <div className='text-muted-foreground space-y-1 text-xs'>
                      <div>
                          {t('accessGroups.card.assignmentMode')}:{' '}
                          {group.channelAssignment.mode || t('accessGroups.card.defaultStrategy')}
                        </div>
                        <div>
                          {t('accessGroups.card.quota')}: {quotaSummary || t('accessGroups.card.noQuota')}
                        </div>
                        {group.channelAssignment.reason && (
                          <div>
                            {t('accessGroups.card.assignmentReason')}: {group.channelAssignment.reason}
                          </div>
                        )}
                      </div>

                      {modelPreview.length > 0 && (
                        <div className='flex flex-wrap gap-1'>
                          {modelPreview.map((modelID) => (
                            <Badge key={`${group.id}-${modelID}`} variant='outline'>
                              {modelID}
                            </Badge>
                          ))}
                        </div>
                      )}
                    </>
                  )}

                  <div className='flex flex-wrap gap-2'>
                    {canEditPolicy && (
                      <Button
                        variant={visible ? 'outline' : 'default'}
                        disabled={updateGroupVisibility.isPending}
                        onClick={() => setGroupVisible(group, !visible)}
                      >
                        {visible ? t('accessGroups.actions.hide') : t('accessGroups.actions.expose')}
                      </Button>
                    )}
                    {channelPermissions.canRead && (
                      <Button
                        variant={selectedGroup?.id === group.id ? 'secondary' : 'outline'}
                        onClick={() => setSelectedGroupId(group.id)}
                      >
                        {canEditPolicy ? t('accessGroups.actions.manageChannels') : t('accessGroups.actions.viewChannels')}
                      </Button>
                    )}
                  </div>
                </CardContent>
              </Card>
            );
          })}
        </div>

        {selectedGroup && channelPermissions.canRead && (
          <Card>
            <CardHeader>
              <div className='flex flex-wrap items-start justify-between gap-3'>
                <div>
                  <CardTitle>{t('accessGroups.channels.title', { name: selectedGroup.name })}</CardTitle>
                  <CardDescription>{t('accessGroups.channels.description')}</CardDescription>
                </div>
                <Badge variant='default'>{t('accessGroups.channels.assignable')}</Badge>
              </div>
            </CardHeader>
            <CardContent className='space-y-4'>
              {selectedGroup.channelAssignment.reason && (
                <div className='text-muted-foreground rounded-md border border-dashed p-3 text-xs'>
                  {selectedGroup.channelAssignment.reason}
                </div>
              )}
              {isLoadingChannels && (
                <div className='text-muted-foreground flex items-center gap-2 text-sm'>
                  <Loader2 className='h-4 w-4 animate-spin' />
                  {t('accessGroups.channels.loading')}
                </div>
              )}
              {channelsError && <p className='text-destructive text-sm'>{t('accessGroups.channels.loadError')}</p>}
              {!isLoadingChannels && !channelsError && channels.length === 0 && (
                <p className='text-muted-foreground text-sm'>{t('accessGroups.channels.empty')}</p>
              )}
              <div className='max-h-[520px] space-y-2 overflow-y-auto pr-1'>
                {channels.map((channel) => {
                  const checked = selectedChannelIds.includes(channel.id);
                  return (
                    <label
                      key={channel.id}
                      className='hover:bg-muted/60 flex cursor-pointer items-start gap-3 rounded-md border p-3 text-sm'
                    >
                      <Checkbox
                        checked={checked}
                        disabled={!canEditPolicy || assignChannels.isPending}
                        onCheckedChange={(value) => toggleChannel(channel.id, Boolean(value))}
                        aria-label={t('accessGroups.channels.selectChannel', { name: channel.name })}
                      />
                      <span className='min-w-0 flex-1'>
                        <span className='block font-medium'>{channel.name}</span>
                        <span className='text-muted-foreground block text-xs'>
                          {channel.type} · {channel.status}
                        </span>
                        {channel.tags?.length ? (
                          <span className='mt-2 flex flex-wrap gap-1'>
                            {channel.tags.slice(0, 5).map((tag) => (
                              <Badge key={`${channel.id}-${tag}`} variant='outline'>
                                {tag}
                              </Badge>
                            ))}
                          </span>
                        ) : null}
                      </span>
                    </label>
                  );
                })}
              </div>
              {canEditPolicy && (
                <div className='flex flex-wrap items-center gap-2'>
                  <Button disabled={assignChannels.isPending} onClick={() => assignChannels.mutate()}>
                    {assignChannels.isPending ? t('accessGroups.channels.saving') : t('accessGroups.channels.save')}
                  </Button>
                  <p className='text-muted-foreground text-xs'>{t('accessGroups.channels.saveHint')}</p>
                </div>
              )}
            </CardContent>
          </Card>
        )}
      </div>}
    </div>
  );
}

function SummaryBox({ label, value }: { label: string; value: string }) {
  return (
    <div className='rounded-md border p-3'>
      <div className='text-muted-foreground text-xs'>{label}</div>
      <div className='text-sm font-medium'>{value}</div>
    </div>
  );
}
