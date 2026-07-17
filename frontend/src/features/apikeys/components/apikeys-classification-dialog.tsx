import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { useSelectedProjectId } from '@/stores/projectStore';
import { usePermissions } from '@/hooks/usePermissions';
import { accessGroupsApi, adminAPIKeyCoherenceApi } from '@/lib/api-client';
import { extractNumberIDAsNumber } from '@/lib/utils';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Label } from '@/components/ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { useApiKey } from '../data/apikeys';
import type { ApiKey } from '../data/schema';
import { formatPolicyList, formatPolicyQuota } from './classification-preview';
import { PolicyComparisonPreview } from './classification-policy-preview';

type ClassificationMode = 'admin' | 'personal_snapshot' | 'personal_access_group';

export function ApiKeyClassificationDialog({
  apiKey,
  open,
  onOpenChange,
}: {
  apiKey: ApiKey;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const projectId = useSelectedProjectId();
  const { apiKeyPermissions, channelPermissions } = usePermissions();
  const canReadAccessGroupPolicy = apiKeyPermissions.canRead && channelPermissions.canRead;
  const [mode, setMode] = useState<ClassificationMode>('admin');
  const [accessGroupId, setAccessGroupId] = useState('');
  const detail = useApiKey(open ? apiKey.id : '');
  const groups = useQuery({
    queryKey: ['adminAccessGroups', projectId],
    queryFn: () => accessGroupsApi.list(projectId!),
    enabled: open && mode === 'personal_access_group' && Boolean(projectId) && canReadAccessGroupPolicy,
  });

  useEffect(() => {
    if (!open) {
      setMode('admin');
      setAccessGroupId('');
    }
  }, [open]);

  const selectedGroup = groups.data?.find((group) => String(group.id) === accessGroupId);
  const currentProfile = useMemo(() => {
    const profiles = detail.data?.profiles?.profiles ?? [];
    return profiles.find((profile) => profile.name === detail.data?.profiles?.activeProfile) ?? profiles[0];
  }, [detail.data]);
  const keepsSnapshot = mode !== 'personal_access_group';
  const currentModels = formatPolicyList(currentProfile?.modelIDs);
  const currentChannels = formatPolicyList([
    ...(currentProfile?.channelIDs ?? []),
    ...(currentProfile?.channelTags ?? []).map((tag) => `#${tag}`),
  ]);
  const comparison = [
    {
      label: t('apikeys.classification.preview.models'),
      current: currentModels,
      target: keepsSnapshot ? currentModels : formatPolicyList(selectedGroup?.profiles[0]?.modelIds),
    },
    {
      label: t('apikeys.classification.preview.channels'),
      current: currentChannels,
      target: keepsSnapshot
        ? currentChannels
        : formatPolicyList([
            ...(selectedGroup?.channelAssignment.channelIds ?? []),
            ...(selectedGroup?.channelAssignment.tags ?? []).map((tag) => `#${tag}`),
          ]),
    },
    {
      label: t('apikeys.classification.preview.routing'),
      current: currentProfile?.loadBalanceStrategy || t('apikeys.classification.preview.defaultRouting'),
      target: keepsSnapshot
        ? currentProfile?.loadBalanceStrategy || t('apikeys.classification.preview.defaultRouting')
        : t('apikeys.classification.preview.liveGroupRouting', {
            name: selectedGroup?.name ?? '-',
            mode: selectedGroup?.channelAssignment.mode || t('apikeys.classification.preview.defaultRouting'),
          }),
    },
    {
      label: t('apikeys.classification.preview.quota'),
      current: formatPolicyQuota(currentProfile?.quota, t),
      target: keepsSnapshot
        ? formatPolicyQuota(currentProfile?.quota, t)
        : formatPolicyQuota(selectedGroup?.profiles[0]?.quotaSummary, t),
    },
  ];

  const classify = useMutation({
    mutationFn: async () => {
      const id = extractNumberIDAsNumber(apiKey.id);
      if (!id) throw new Error(t('apikeys.classification.invalidId'));
      if (mode === 'personal_access_group' && !accessGroupId) {
        throw new Error(t('apikeys.classification.groupRequired'));
      }
      if (mode === 'personal_access_group' && !canReadAccessGroupPolicy) {
        throw new Error(t('apikeys.classification.channelReadRequired'));
      }
      return adminAPIKeyCoherenceApi.classifyLegacy(id, {
        mode,
        ...(mode === 'personal_access_group' ? { accessGroupId } : {}),
      });
    },
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['apiKeys'] }),
        queryClient.invalidateQueries({ queryKey: ['apiKey', apiKey.id] }),
      ]);
      toast.success(t('apikeys.classification.success'));
      onOpenChange(false);
    },
    onError: (error: Error) => toast.error(error.message || t('apikeys.classification.failed')),
  });

  const previewUnavailable = detail.isLoading || detail.isError || (mode === 'personal_access_group' && groups.isError);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='max-h-[90vh] overflow-y-auto sm:max-w-2xl'>
        <DialogHeader>
          <DialogTitle>{t('apikeys.classification.title')}</DialogTitle>
          <DialogDescription>{t('apikeys.classification.description', { name: apiKey.name })}</DialogDescription>
        </DialogHeader>

        <div className='grid gap-4 sm:grid-cols-2'>
          <div className='space-y-2'>
            <Label htmlFor='legacy-classification-mode'>{t('apikeys.classification.modeLabel')}</Label>
            <Select value={mode} onValueChange={(value: ClassificationMode) => setMode(value)}>
              <SelectTrigger id='legacy-classification-mode' className='w-full'>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value='admin'>{t('apikeys.classification.mode.admin')}</SelectItem>
                <SelectItem value='personal_snapshot'>{t('apikeys.classification.mode.personalSnapshot')}</SelectItem>
                <SelectItem value='personal_access_group' disabled={!canReadAccessGroupPolicy}>
                  {t('apikeys.classification.mode.personalAccessGroup')}
                </SelectItem>
              </SelectContent>
            </Select>
          </div>
          {mode === 'personal_access_group' && (
            <div className='space-y-2'>
              <Label htmlFor='legacy-access-group'>{t('apikeys.classification.groupLabel')}</Label>
              <Select value={accessGroupId} onValueChange={setAccessGroupId}>
                <SelectTrigger id='legacy-access-group' className='w-full'>
                  <SelectValue placeholder={t('apikeys.classification.groupPlaceholder')} />
                </SelectTrigger>
                <SelectContent>
                  {groups.data?.map((group) => (
                    <SelectItem key={group.id} value={String(group.id)} disabled={!group.enabled}>
                      {group.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          )}
        </div>

        {(detail.isError || groups.isError || classify.error) && (
          <div role='alert' className='border-destructive/40 bg-destructive/5 text-destructive rounded-md border p-3 text-sm'>
            {(classify.error as Error | null)?.message || (detail.error as Error | null)?.message || (groups.error as Error | null)?.message || t('apikeys.classification.previewError')}
          </div>
        )}

        <PolicyComparisonPreview rows={comparison} />

        <DialogFooter>
          <Button variant='outline' onClick={() => onOpenChange(false)}>{t('common.buttons.cancel')}</Button>
          <Button
            disabled={classify.isPending || previewUnavailable || (mode === 'personal_access_group' && !selectedGroup)}
            onClick={() => classify.mutate()}
          >
            {classify.isPending ? t('apikeys.classification.saving') : t('apikeys.classification.confirm')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
