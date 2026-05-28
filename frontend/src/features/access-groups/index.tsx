import { useMemo } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Loader2, ShieldCheck } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { useMyProjects } from '@/features/projects/data/projects';
import { accessGroupsApi, authApi, type AdminAccessGroup, type AdminRegistrationPolicy, type SelfAccessGroupProfile } from '@/lib/api-client';
import { useSelectedProjectId } from '@/stores/projectStore';

const WILDCARD_PRESET = '*';

function toUpdateInput(policy: AdminRegistrationPolicy, selfServicePresetNames: string[]) {
  return {
    enabled: policy.enabled,
    oidcEnabled: policy.oidcEnabled,
    selfServiceEnabled: policy.selfServiceEnabled,
    inviteCode: policy.inviteCode,
    defaultProjectId: policy.defaultProjectId,
    autoJoinFirstProject: policy.autoJoinFirstProject,
    defaultProjectScopes: policy.defaultProjectScopes,
    allowRequestDetails: policy.allowRequestDetails,
    selfServicePresetNames,
  };
}

function describeQuota(profile?: SelfAccessGroupProfile) {
  const quota = profile?.quotaSummary;
  if (!quota) return '';

  const parts = [
    quota.requests ? `${quota.requests.toLocaleString()} requests` : '',
    quota.totalTokens ? `${quota.totalTokens.toLocaleString()} tokens` : '',
    quota.cost ? `cost ${quota.cost}` : '',
  ].filter(Boolean);

  return parts.join(' · ');
}

function primaryProfile(group: AdminAccessGroup) {
  return group.profiles[0];
}

export default function AccessGroups() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const selectedProjectId = useSelectedProjectId();
  const { data: myProjects } = useMyProjects();
  const projectId = selectedProjectId || null;
  const projectName = myProjects?.find((project) => project.id === projectId)?.name;

  const { data: accessGroups, isLoading: isLoadingGroups, isError: accessGroupsError } = useQuery({
    queryKey: ['adminAccessGroups', projectId],
    queryFn: async () => accessGroupsApi.list(projectId!),
    enabled: Boolean(projectId),
  });
  const { data: policy, isLoading: isLoadingPolicy, isError: policyError } = useQuery({
    queryKey: ['adminRegistrationPolicy'],
    queryFn: async () => (await authApi.adminRegistrationPolicy()).data,
  });

  const exposedNames = useMemo(() => new Set(policy?.selfServicePresetNames ?? []), [policy?.selfServicePresetNames]);
  const exposesAll = exposedNames.has(WILDCARD_PRESET);
  const explicitGroupNames = useMemo(() => (accessGroups ?? []).map((group) => group.name), [accessGroups]);

  const updatePolicy = useMutation({
    mutationFn: async (selfServicePresetNames: string[]) => {
      if (!policy) {
        throw new Error(t('accessGroups.errors.policyMissing'));
      }
      return authApi.updateAdminRegistrationPolicy(toUpdateInput(policy, selfServicePresetNames));
    },
    onSuccess: (response) => {
      queryClient.setQueryData(['adminRegistrationPolicy'], response.data);
      toast.success(t('accessGroups.toasts.updated'));
    },
    onError: (error: Error) => toast.error(error.message || t('accessGroups.toasts.updateFailed')),
  });

  const setTemplateVisible = (templateName: string, visible: boolean) => {
    if (!policy) return;

    if (exposesAll) {
      const nextNames = visible ? explicitGroupNames : explicitGroupNames.filter((name) => name !== templateName);
      updatePolicy.mutate(nextNames);
      return;
    }

    const current = policy.selfServicePresetNames.filter((name) => name !== WILDCARD_PRESET);
    const nextNames = visible
      ? Array.from(new Set([...current, templateName])).sort((a, b) => a.localeCompare(b))
      : current.filter((name) => name !== templateName);
    updatePolicy.mutate(nextNames);
  };

  const makeExplicit = () => updatePolicy.mutate(explicitGroupNames);

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

  const isLoading = isLoadingGroups || isLoadingPolicy;
  const isError = accessGroupsError || policyError;

  return (
    <div className='space-y-6 p-6'>
      <div className='flex flex-wrap items-start justify-between gap-4'>
        <div className='space-y-2'>
          <h1 className='text-2xl font-semibold tracking-tight'>{t('accessGroups.title')}</h1>
          <p className='text-muted-foreground max-w-3xl text-sm'>{t('accessGroups.description')}</p>
          <div className='flex flex-wrap gap-2'>
            <Badge variant={policy?.selfServiceEnabled ? 'default' : 'secondary'}>
              {policy?.selfServiceEnabled ? t('accessGroups.badges.selfServiceOn') : t('accessGroups.badges.selfServiceOff')}
            </Badge>
            <Badge variant='outline'>{projectName || t('accessGroups.badges.currentProject')}</Badge>
            {exposesAll && <Badge variant='secondary'>{t('accessGroups.badges.wildcard')}</Badge>}
          </div>
        </div>
        {exposesAll && (
          <Button variant='outline' disabled={updatePolicy.isPending || !accessGroups?.length} onClick={makeExplicit}>
            {updatePolicy.isPending ? <Loader2 className='h-4 w-4 animate-spin' /> : <ShieldCheck className='h-4 w-4' />}
            {t('accessGroups.actions.makeExplicit')}
          </Button>
        )}
      </div>

      <Card className='border-dashed'>
        <CardHeader>
          <CardTitle>{t('accessGroups.compat.title')}</CardTitle>
          <CardDescription>{t('accessGroups.compat.description')}</CardDescription>
        </CardHeader>
        <CardContent className='text-muted-foreground text-sm'>{t('accessGroups.compat.channelAssignment')}</CardContent>
      </Card>

      {isLoading && (
        <div className='flex h-32 items-center justify-center'>
          <Loader2 className='h-6 w-6 animate-spin' />
          <span className='text-muted-foreground ml-2'>{t('common.loading')}</span>
        </div>
      )}

      {isError && (
        <Card>
          <CardHeader>
            <CardTitle>{t('accessGroups.errors.loadTitle')}</CardTitle>
            <CardDescription>{t('accessGroups.errors.loadDescription')}</CardDescription>
          </CardHeader>
        </Card>
      )}

      {!isLoading && !isError && !accessGroups?.length && (
        <Card>
          <CardHeader>
            <CardTitle>{t('accessGroups.empty.title')}</CardTitle>
            <CardDescription>{t('accessGroups.empty.description')}</CardDescription>
          </CardHeader>
        </Card>
      )}

      <div className='grid gap-4 lg:grid-cols-2'>
        {accessGroups?.map((group) => {
          const visible = group.selfServiceVisible;
          const profile = primaryProfile(group);
          const modelCount = profile?.modelCount ?? 0;
          const tagCount = group.channelAssignment.tags?.length ?? 0;
          const quotaSummary = describeQuota(profile);
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
                <div className='grid gap-2 text-sm sm:grid-cols-3'>
                  <SummaryBox label={t('accessGroups.card.models')} value={modelCount ? String(modelCount) : t('accessGroups.card.anyModel')} />
                  <SummaryBox
                    label={t('accessGroups.card.channels')}
                    value={group.channelAssignment.channelCount ? String(group.channelAssignment.channelCount) : t('accessGroups.card.byTags')}
                  />
                  <SummaryBox label={t('accessGroups.card.tags')} value={tagCount ? String(tagCount) : t('accessGroups.card.noTags')} />
                </div>

                <div className='text-muted-foreground space-y-1 text-xs'>
                  <div>
                    {t('accessGroups.card.profile')}: <span className='text-foreground'>{profile?.name || group.name}</span>
                  </div>
                  <div>
                    {t('accessGroups.card.assignmentMode')}: {group.channelAssignment.mode || t('accessGroups.card.defaultStrategy')}
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

                <div className='flex flex-wrap gap-2'>
                  <Button
                    variant={visible ? 'outline' : 'default'}
                    disabled={updatePolicy.isPending}
                    onClick={() => setTemplateVisible(group.name, !visible)}
                  >
                    {visible ? t('accessGroups.actions.hide') : t('accessGroups.actions.expose')}
                  </Button>
                </div>
              </CardContent>
            </Card>
          );
        })}
      </div>
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
