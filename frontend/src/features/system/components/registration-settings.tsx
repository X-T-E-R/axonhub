'use client';

import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Loader2, RotateCcw, Save } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Checkbox } from '@/components/ui/checkbox';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Separator } from '@/components/ui/separator';
import { Switch } from '@/components/ui/switch';
import { Textarea } from '@/components/ui/textarea';
import { authApi, type AdminRegistrationPolicy } from '@/lib/api-client';
import i18n from '@/lib/i18n';

type RegistrationPolicyForm = Omit<AdminRegistrationPolicy, 'inviteCodeRequired' | 'passwordSignupAllowed'>;

const RECOMMENDED_PROJECT_SCOPES: string[] = [];
const DEFAULT_SELF_SERVICE_PRESETS: string[] = [];

const PROJECT_SCOPE_OPTIONS = [
  { value: 'read_requests' },
  { value: 'write_requests' },
  { value: 'read_api_keys' },
  { value: 'write_api_keys' },
  { value: 'read_prompts' },
  { value: 'write_prompts' },
  { value: 'read_users' },
  { value: 'write_users' },
  { value: 'read_roles' },
  { value: 'write_roles' },
  { value: 'write_channels' },
  { value: 'read_dashboard' },
  { value: 'read_projects' },
  { value: 'write_projects' },
  { value: 'read_settings' },
  { value: 'write_settings' },
] as const;

const parseList = (value: string) =>
  value
    .split(/[\n,]+/)
    .map((item) => item.trim())
    .filter(Boolean);

const stringifyList = (value: string[]) => value.join('\n');

function toForm(policy: AdminRegistrationPolicy): RegistrationPolicyForm {
  return {
    enabled: policy.enabled,
    oidcEnabled: policy.oidcEnabled,
    selfServiceEnabled: policy.selfServiceEnabled ?? false,
    inviteCode: policy.inviteCode || '',
    defaultProjectId: policy.defaultProjectId || 0,
    autoJoinFirstProject: policy.autoJoinFirstProject,
    defaultProjectScopes: policy.defaultProjectScopes || [],
    allowRequestDetails: policy.allowRequestDetails,
    selfServicePresetNames: policy.selfServicePresetNames || [],
  };
}

function withUsableOnboardingDefaults(form: RegistrationPolicyForm): RegistrationPolicyForm {
  return {
    ...form,
    autoJoinFirstProject: form.defaultProjectId > 0 ? form.autoJoinFirstProject : true,
    defaultProjectScopes: form.defaultProjectScopes.length > 0 ? form.defaultProjectScopes : [...RECOMMENDED_PROJECT_SCOPES],
    selfServicePresetNames: form.selfServicePresetNames.length > 0 ? form.selfServicePresetNames : [...DEFAULT_SELF_SERVICE_PRESETS],
  };
}

const initialForm: RegistrationPolicyForm = {
  enabled: false,
  oidcEnabled: false,
  selfServiceEnabled: false,
  inviteCode: '',
  defaultProjectId: 0,
  autoJoinFirstProject: true,
  defaultProjectScopes: [...RECOMMENDED_PROJECT_SCOPES],
  allowRequestDetails: false,
  selfServicePresetNames: [...DEFAULT_SELF_SERVICE_PRESETS],
};

export function RegistrationSettings() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [form, setForm] = useState<RegistrationPolicyForm>(initialForm);
  const [presetNamesText, setPresetNamesText] = useState('');

  const { data: policy, isLoading } = useQuery({
    queryKey: ['adminRegistrationPolicy'],
    queryFn: async () => (await authApi.adminRegistrationPolicy()).data,
  });

  const updatePolicy = useMutation({
    mutationFn: authApi.updateAdminRegistrationPolicy,
    onSuccess: (response) => {
      queryClient.setQueryData(['adminRegistrationPolicy'], response.data);
      toast.success(i18n.t('common.success.systemUpdated'));
    },
    onError: () => {
      toast.error(i18n.t('common.errors.systemUpdateFailed'));
    },
  });

  useEffect(() => {
    if (!policy) {
      return;
    }

    setForm(toForm(policy));
    setPresetNamesText(stringifyList(policy.selfServicePresetNames || []));
  }, [policy]);

  const nextForm = useMemo(
    () => ({
      ...form,
      inviteCode: form.inviteCode.trim(),
      selfServicePresetNames: parseList(presetNamesText),
    }),
    [form, presetNamesText],
  );

  const hasChanges = useMemo(() => {
    if (!policy) {
      return false;
    }

    return JSON.stringify(toForm(policy)) !== JSON.stringify(nextForm);
  }, [nextForm, policy]);

  const exposesAllPresets = nextForm.selfServicePresetNames.includes('*');

  const handleEnableMode = (field: 'enabled' | 'oidcEnabled', enabled: boolean) => {
    setForm((prev) => {
      const next = { ...prev, [field]: enabled };
      return enabled ? withUsableOnboardingDefaults(next) : next;
    });
  };

  const handleScopeToggle = (scope: string, checked: boolean) => {
    setForm((prev) => ({
      ...prev,
      defaultProjectScopes: checked
        ? Array.from(new Set([...prev.defaultProjectScopes, scope]))
        : prev.defaultProjectScopes.filter((item) => item !== scope),
    }));
  };

  const restoreRecommendedDefaults = () => {
    setForm((prev) =>
      withUsableOnboardingDefaults({
        ...prev,
        defaultProjectScopes: [...RECOMMENDED_PROJECT_SCOPES],
      }),
    );
  };

  const handleSave = async () => {
    await updatePolicy.mutateAsync(nextForm);
  };

  if (isLoading) {
    return (
      <div className='flex h-32 items-center justify-center'>
        <Loader2 className='h-6 w-6 animate-spin' />
        <span className='text-muted-foreground ml-2'>{t('common.loading')}</span>
      </div>
    );
  }

  return (
    <div className='space-y-6'>
      <Card>
        <CardHeader>
          <div className='flex flex-wrap items-start justify-between gap-3'>
            <div>
              <CardTitle>{t('system.registration.onboardingTitle')}</CardTitle>
              <CardDescription>{t('system.registration.onboardingDescription')}</CardDescription>
            </div>
            <Badge variant={form.enabled || form.oidcEnabled ? 'default' : 'secondary'}>
              {form.enabled || form.oidcEnabled ? t('system.registration.status.open') : t('system.registration.status.closed')}
            </Badge>
          </div>
        </CardHeader>
        <CardContent className='space-y-6'>
          <div className='flex items-center justify-between gap-4'>
            <div className='space-y-0.5'>
              <Label htmlFor='registration-enabled'>{t('system.registration.passwordSignup.label')}</Label>
              <div className='text-muted-foreground text-sm'>{t('system.registration.passwordSignup.description')}</div>
            </div>
            <Switch id='registration-enabled' checked={form.enabled} onCheckedChange={(enabled) => handleEnableMode('enabled', enabled)} />
          </div>

          {policy && !policy.passwordSignupAllowed && (
            <div className='text-muted-foreground rounded-lg border border-dashed p-3 text-sm'>
              {t('system.registration.passwordSignup.oidcOnlyHint')}
            </div>
          )}

          <div className='flex items-center justify-between gap-4'>
            <div className='space-y-0.5'>
              <Label htmlFor='registration-oidc'>{t('system.registration.oidcSignup.label')}</Label>
              <div className='text-muted-foreground text-sm'>{t('system.registration.oidcSignup.description')}</div>
            </div>
            <Switch
              id='registration-oidc'
              checked={form.oidcEnabled}
              onCheckedChange={(oidcEnabled) => handleEnableMode('oidcEnabled', oidcEnabled)}
            />
          </div>

          <Separator />

          <div className='grid gap-4 md:grid-cols-2'>
            <div className='space-y-2'>
              <Label htmlFor='registration-invite-code'>{t('system.registration.inviteCode.label')}</Label>
              <Input
                id='registration-invite-code'
                value={form.inviteCode}
                onChange={(event) =>
                  setForm((prev) => ({
                    ...prev,
                    inviteCode: event.target.value,
                  }))
                }
                placeholder={t('system.registration.inviteCode.placeholder')}
              />
              <div className='text-muted-foreground text-sm'>{t('system.registration.inviteCode.description')}</div>
            </div>

            <div className='space-y-2'>
              <Label htmlFor='registration-default-project'>{t('system.registration.defaultProject.label')}</Label>
              <Input
                id='registration-default-project'
                type='number'
                min={0}
                value={form.defaultProjectId}
                onChange={(event) =>
                  setForm((prev) => ({
                    ...prev,
                    defaultProjectId: Math.max(0, Number(event.target.value) || 0),
                  }))
                }
              />
              <div className='text-muted-foreground text-sm'>{t('system.registration.defaultProject.description')}</div>
            </div>
          </div>

          <div className='flex items-center justify-between gap-4'>
            <div className='space-y-0.5'>
              <Label htmlFor='registration-auto-project'>{t('system.registration.autoJoinFirstProject.label')}</Label>
              <div className='text-muted-foreground text-sm'>{t('system.registration.autoJoinFirstProject.description')}</div>
            </div>
            <Switch
              id='registration-auto-project'
              checked={form.autoJoinFirstProject}
              onCheckedChange={(autoJoinFirstProject) => setForm((prev) => ({ ...prev, autoJoinFirstProject }))}
            />
          </div>

          <Separator />

          <div className='space-y-3'>
            <div className='flex flex-wrap items-center justify-between gap-2'>
              <div className='space-y-1'>
                <Label>{t('system.registration.defaultScopes.label')}</Label>
                <div className='text-muted-foreground text-sm'>{t('system.registration.defaultScopes.description')}</div>
              </div>
              <Button type='button' variant='outline' size='sm' onClick={restoreRecommendedDefaults}>
                <RotateCcw className='mr-2 h-4 w-4' />
                {t('system.registration.defaultScopes.restoreRecommended')}
              </Button>
            </div>

            <div className='grid gap-2 sm:grid-cols-2 xl:grid-cols-3'>
              {PROJECT_SCOPE_OPTIONS.map((option) => {
                const checked = form.defaultProjectScopes.includes(option.value);
                return (
                  <label
                    key={option.value}
                    htmlFor={`registration-scope-${option.value}`}
                    className={`flex min-h-14 cursor-pointer items-start gap-3 rounded-lg border p-3 text-sm transition-colors ${
                      checked ? 'border-primary/50 bg-primary/5' : 'hover:bg-muted/50'
                    }`}
                  >
                    <Checkbox
                      id={`registration-scope-${option.value}`}
                      checked={checked}
                      onCheckedChange={(value) => handleScopeToggle(option.value, value === true)}
                      className='mt-0.5'
                    />
                    <span className='space-y-1'>
                      <span className='block font-medium'>{t(`scopes.${option.value}`)}</span>
                    </span>
                  </label>
                );
              })}
            </div>

            <div className='text-muted-foreground rounded-lg border border-dashed p-3 text-sm'>
              {t('system.registration.defaultScopes.safeHint')}
            </div>
          </div>

          <Separator />

          <div className='space-y-3'>
            <div className='flex flex-wrap items-start justify-between gap-3'>
              <div className='space-y-1'>
                <Label>{t('system.registration.selfService.title')}</Label>
                <div className='text-muted-foreground text-sm'>{t('system.registration.selfService.description')}</div>
              </div>
              <Badge variant={form.selfServiceEnabled ? 'default' : 'secondary'}>
                {form.selfServiceEnabled
                  ? t('system.registration.selfService.status.enabled')
                  : t('system.registration.selfService.status.disabled')}
              </Badge>
            </div>

            <div className='flex items-center justify-between gap-4 rounded-lg border p-4'>
              <div className='space-y-0.5'>
                <Label htmlFor='self-service-enabled'>{t('system.registration.selfService.enabled.label')}</Label>
                <div className='text-muted-foreground text-sm'>{t('system.registration.selfService.enabled.description')}</div>
              </div>
              <Switch
                id='self-service-enabled'
                checked={form.selfServiceEnabled ?? false}
                onCheckedChange={(selfServiceEnabled) => setForm((prev) => ({ ...prev, selfServiceEnabled }))}
              />
            </div>
          </div>

          <div className='grid gap-4 md:grid-cols-2'>
            <div className='space-y-2'>
              <Label htmlFor='registration-preset-names'>{t('system.registration.selfServicePresets.label')}</Label>
              <Textarea
                id='registration-preset-names'
                value={presetNamesText}
                onChange={(event) => setPresetNamesText(event.target.value)}
                placeholder={'fast\ncheap'}
                className='min-h-28'
              />
              <div className='text-muted-foreground text-sm'>{t('system.registration.selfServicePresets.description')}</div>
              {!nextForm.selfServicePresetNames.length && (
                <div className='rounded-lg border border-dashed p-3 text-sm'>
                  {t('system.registration.selfServicePresets.noneSelected')}
                </div>
              )}
              {exposesAllPresets && (
                <div className='rounded-lg border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/30 dark:text-amber-200'>
                  {t('system.registration.selfServicePresets.wildcardWarning')}
                </div>
              )}
            </div>

            <div className='flex items-center justify-between gap-4 rounded-lg border p-4'>
              <div className='space-y-0.5'>
                <Label htmlFor='registration-request-details'>{t('system.registration.requestDetails.label')}</Label>
                <div className='text-muted-foreground text-sm'>{t('system.registration.requestDetails.description')}</div>
              </div>
              <Switch
                id='registration-request-details'
                checked={form.allowRequestDetails}
                onCheckedChange={(allowRequestDetails) => setForm((prev) => ({ ...prev, allowRequestDetails }))}
              />
            </div>
          </div>
        </CardContent>
      </Card>

      {hasChanges && (
        <div className='flex justify-end'>
          <Button onClick={handleSave} disabled={updatePolicy.isPending} className='min-w-24'>
            {updatePolicy.isPending ? (
              <Loader2 className='h-4 w-4 animate-spin' />
            ) : (
              <>
                <Save className='mr-2 h-4 w-4' />
                {t('common.buttons.save')}
              </>
            )}
          </Button>
        </div>
      )}
    </div>
  );
}
