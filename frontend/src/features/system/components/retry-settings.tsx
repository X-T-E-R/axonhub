'use client';

import React, { useState, useEffect, useCallback } from 'react';
import { Loader2, Plus, Trash2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Checkbox } from '@/components/ui/checkbox';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Separator } from '@/components/ui/separator';
import { Switch } from '@/components/ui/switch';
import { Textarea } from '@/components/ui/textarea';
import type { FailurePolicyActionType, FailurePolicyEventSource, FailurePolicyProfile } from '@/features/channels/data/schema';
import { useRetryPolicy, useUpdateRetryPolicy, type RetryPolicyInput } from '../data/system';

type FailurePolicyTarget = 'key' | 'channel';

const FAILURE_EVENT_SOURCES: FailurePolicyEventSource[] = [
  'request_failure',
  'scheduled_health_check_failure',
  'manual_health_check_failure',
];
const KEY_FAILURE_ACTIONS: FailurePolicyActionType[] = ['report_only', 'backoff_key', 'disable_key', 'archive_key', 'delete_key'];
const CHANNEL_FAILURE_ACTIONS: FailurePolicyActionType[] = ['report_only', 'disable_channel'];

function parseStatusList(input: string): number[] {
  return input
    .split(',')
    .map((item) => Number(item.trim()))
    .filter((item) => Number.isInteger(item) && item >= 100 && item <= 599);
}

function createDefaultProfile(index: number, target: FailurePolicyTarget): FailurePolicyProfile {
  return {
    id: `${target}-policy-${Date.now()}-${index + 1}`,
    name: `${target === 'key' ? 'Key' : 'Channel'} policy ${index + 1}`,
    enabled: true,
    sources: target === 'key' ? ['request_failure'] : ['request_failure'],
    conditions: {
      minFailureCount: 3,
      statusCodes: [],
      available: null,
      balanceLTE: null,
      reasonContains: null,
      allCheckedKeysFailed: null,
      expr: null,
    },
    actions: [
      {
        type: 'report_only',
        backoff: null,
      },
    ],
  };
}

function createDefaultAction(target: FailurePolicyTarget): NonNullable<FailurePolicyProfile['actions']>[number] {
  return {
    type: target === 'key' ? 'report_only' : 'report_only',
    backoff: null,
  };
}

function normalizeFailurePolicyProfileSources(profile: FailurePolicyProfile, target: FailurePolicyTarget): FailurePolicyProfile {
  return {
    ...profile,
    sources: profile.sources && profile.sources.length > 0 ? profile.sources : target === 'key' ? ['request_failure'] : ['request_failure'],
  };
}

function normalizeFailurePolicySources(policy: RetryPolicyInput['failurePolicy']): RetryPolicyInput['failurePolicy'] {
  return {
    keyProfiles: (policy?.keyProfiles || []).map((profile) => normalizeFailurePolicyProfileSources(profile, 'key')),
    channelProfiles: (policy?.channelProfiles || []).map((profile) => normalizeFailurePolicyProfileSources(profile, 'channel')),
  };
}

export function RetrySettings() {
  const { t } = useTranslation();
  const { data: retryPolicy, isLoading } = useRetryPolicy();
  const updateRetryPolicy = useUpdateRetryPolicy();

  const [formData, setFormData] = useState<RetryPolicyInput>({
    enabled: true,
    maxChannelRetries: 3,
    maxSingleChannelRetries: 2,
    retryDelayMs: 1000,
    streamFirstEventTimeoutSeconds: 0,
    nonStreamResponseTimeoutSeconds: 0,
    loadBalancerStrategy: 'adaptive',
    emptyResponseDetection: false,
    emptyResponseTextPatterns: [],
    upstreamErrorPolicy: {
      mode: 'passthrough',
      customMessage: '',
    },
    autoDisableChannel: {
      enabled: false,
      statuses: [],
    },
    failurePolicy: {
      keyProfiles: [],
      channelProfiles: [],
    },
  });

  useEffect(() => {
    if (retryPolicy) {
      setFormData({
        enabled: retryPolicy.enabled,
        maxChannelRetries: retryPolicy.maxChannelRetries,
        maxSingleChannelRetries: retryPolicy.maxSingleChannelRetries,
        retryDelayMs: retryPolicy.retryDelayMs,
        streamFirstEventTimeoutSeconds: retryPolicy.streamFirstEventTimeoutSeconds,
        nonStreamResponseTimeoutSeconds: retryPolicy.nonStreamResponseTimeoutSeconds,
        loadBalancerStrategy: retryPolicy.loadBalancerStrategy,
        emptyResponseDetection: retryPolicy.emptyResponseDetection,
        emptyResponseTextPatterns: retryPolicy.emptyResponseTextPatterns || [],
        upstreamErrorPolicy: {
          mode: retryPolicy.upstreamErrorPolicy?.mode || 'passthrough',
          customMessage: retryPolicy.upstreamErrorPolicy?.customMessage || '',
        },
        autoDisableChannel: {
          enabled: retryPolicy.autoDisableChannel?.enabled || false,
          statuses: retryPolicy.autoDisableChannel?.statuses || [],
        },
        failurePolicy: {
          keyProfiles: (retryPolicy.failurePolicy?.keyProfiles || []).map((profile) =>
            normalizeFailurePolicyProfileSources(profile, 'key')
          ),
          channelProfiles: (retryPolicy.failurePolicy?.channelProfiles || []).map((profile) =>
            normalizeFailurePolicyProfileSources(profile, 'channel')
          ),
        },
      });
    }
  }, [retryPolicy]);

  const handleInputChange = useCallback((field: keyof RetryPolicyInput, value: string | boolean | number | string[]) => {
    setFormData((prev) => ({
      ...prev,
      [field]: value,
    }));
  }, []);

  const handleEmptyResponseTextPatternsChange = useCallback(
    (value: string) => {
      handleInputChange(
        'emptyResponseTextPatterns',
        value.split('\n').map((line) => line.trim())
      );
    },
    [handleInputChange]
  );

  const handleUpstreamErrorPolicyChange = useCallback((field: 'mode' | 'customMessage', value: string) => {
    setFormData((prev) => ({
      ...prev,
      upstreamErrorPolicy: {
        ...prev.upstreamErrorPolicy,
        [field]: value,
      },
    }));
  }, []);

  const handleAutoDisableChannelChange = useCallback((field: 'enabled', value: boolean) => {
    setFormData((prev) => ({
      ...prev,
      autoDisableChannel: {
        ...prev.autoDisableChannel,
        [field]: value,
      },
    }));
  }, []);

  const handleStatusChange = useCallback((index: number, field: 'status' | 'times', value: number) => {
    setFormData((prev) => ({
      ...prev,
      autoDisableChannel: {
        ...prev.autoDisableChannel,
        statuses: prev.autoDisableChannel?.statuses?.map((s, i) => (i === index ? { ...s, [field]: value } : s)) || [],
      },
    }));
  }, []);

  const addStatus = useCallback(() => {
    setFormData((prev) => ({
      ...prev,
      autoDisableChannel: {
        ...prev.autoDisableChannel,
        statuses: [...(prev.autoDisableChannel?.statuses || []), { status: 500, times: 3 }],
      },
    }));
  }, []);

  const removeStatus = useCallback((index: number) => {
    setFormData((prev) => ({
      ...prev,
      autoDisableChannel: {
        ...prev.autoDisableChannel,
        statuses: prev.autoDisableChannel?.statuses?.filter((_, i) => i !== index) || [],
      },
    }));
  }, []);

  const handleFailureProfilesChange = useCallback((target: FailurePolicyTarget, profiles: FailurePolicyProfile[]) => {
    setFormData((prev) => ({
      ...prev,
      failurePolicy: {
        keyProfiles: target === 'key' ? profiles : prev.failurePolicy?.keyProfiles || [],
        channelProfiles: target === 'channel' ? profiles : prev.failurePolicy?.channelProfiles || [],
      },
    }));
  }, []);

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      await updateRetryPolicy.mutateAsync({
        ...formData,
        failurePolicy: normalizeFailurePolicySources(formData.failurePolicy),
      });
    },
    [updateRetryPolicy, formData]
  );

  if (isLoading) {
    return (
      <div className='flex items-center justify-center p-8'>
        <Loader2 className='h-8 w-8 animate-spin' />
      </div>
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('system.retry.title')}</CardTitle>
        <CardDescription>{t('system.retry.description')}</CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={handleSubmit} className='space-y-6'>
          {/* Enable/Disable Retry */}
          <div className='flex items-center justify-between' id='retry-enabled-switch'>
            <div className='space-y-0.5'>
              <Label htmlFor='retry-enabled' className='text-base'>
                {t('system.retry.enabled.label')}
              </Label>
              <div className='text-muted-foreground text-sm'>{t('system.retry.enabled.description')}</div>
            </div>
            <Switch id='retry-enabled' checked={formData.enabled} onCheckedChange={(checked) => handleInputChange('enabled', checked)} />
          </div>

          <Separator />

          <div className='space-y-4'>
            <div className='space-y-2'>
              <Label htmlFor='upstream-error-mode'>{t('system.retry.upstreamErrorPolicy.label')}</Label>
              <div className='text-muted-foreground mb-2 text-sm'>{t('system.retry.upstreamErrorPolicy.description')}</div>
              <Select
                value={formData.upstreamErrorPolicy?.mode || 'passthrough'}
                onValueChange={(value) => value && handleUpstreamErrorPolicyChange('mode', value)}
              >
                <SelectTrigger id='upstream-error-mode' className='w-56'>
                  <SelectValue placeholder={t('system.retry.upstreamErrorPolicy.placeholder')} />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value='passthrough'>{t('system.retry.upstreamErrorPolicy.options.passthrough')}</SelectItem>
                  <SelectItem value='hidden'>{t('system.retry.upstreamErrorPolicy.options.hidden')}</SelectItem>
                  <SelectItem value='custom'>{t('system.retry.upstreamErrorPolicy.options.custom')}</SelectItem>
                </SelectContent>
              </Select>
            </div>

            {formData.upstreamErrorPolicy?.mode === 'custom' && (
              <div className='space-y-2'>
                <Label htmlFor='upstream-error-custom-message'>{t('system.retry.upstreamErrorPolicy.customMessage.label')}</Label>
                <Textarea
                  id='upstream-error-custom-message'
                  value={formData.upstreamErrorPolicy?.customMessage || ''}
                  onChange={(e) => handleUpstreamErrorPolicyChange('customMessage', e.target.value)}
                  placeholder={t('system.retry.upstreamErrorPolicy.customMessage.placeholder')}
                  className='min-h-20'
                />
              </div>
            )}
          </div>

          <Separator />

          {/* Retry Configuration - Only show when enabled */}
          {formData.enabled && (
            <div className='space-y-4'>
              <div className='space-y-2'>
                <Label htmlFor='load-balancer-strategy'>{t('system.retry.loadBalancerStrategy.label')}</Label>
                <div className='text-muted-foreground mb-2 text-sm'>{t('system.retry.loadBalancerStrategy.description')}</div>
                <Select
                  value={formData.loadBalancerStrategy || 'adaptive'}
                  onValueChange={(value) => value && handleInputChange('loadBalancerStrategy', value)}
                >
                  <SelectTrigger id='load-balancer-strategy' className='w-56'>
                    <SelectValue placeholder={t('system.retry.loadBalancerStrategy.placeholder')} />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value='adaptive'>{t('system.retry.loadBalancerStrategy.options.adaptive')}</SelectItem>
                    <SelectItem value='failover'>{t('system.retry.loadBalancerStrategy.options.failover')}</SelectItem>
                    <SelectItem value='circuit-breaker'>{t('system.retry.loadBalancerStrategy.options.circuitBreaker')}</SelectItem>
                    <SelectItem value='round-robin'>{t('system.retry.loadBalancerStrategy.options.roundRobin')}</SelectItem>
                  </SelectContent>
                </Select>

                {/* Strategy Documentation */}
                {formData.loadBalancerStrategy && (
                  <div className='bg-muted/50 mt-3 rounded-md border p-3'>
                    <div className='text-muted-foreground text-xs leading-relaxed'>
                      {t(`system.retry.loadBalancerStrategy.documentation.${formData.loadBalancerStrategy}`)}
                    </div>
                  </div>
                )}
              </div>

              {/* Max Channel Retries */}
              <div className='space-y-2' id='retry-max-retries'>
                <Label htmlFor='max-channel-retries'>{t('system.retry.maxChannelRetries.label')}</Label>
                <div className='text-muted-foreground mb-2 text-sm'>{t('system.retry.maxChannelRetries.description')}</div>
                <Input
                  id='max-channel-retries'
                  type='number'
                  min='0'
                  max='10'
                  value={formData.maxChannelRetries}
                  onChange={(e) => handleInputChange('maxChannelRetries', parseInt(e.target.value) || 0)}
                  className='w-32'
                />
              </div>

              {/* Max Single Channel Retries */}
              <div className='space-y-2'>
                <Label htmlFor='max-single-channel-retries'>{t('system.retry.maxSingleChannelRetries.label')}</Label>
                <div className='text-muted-foreground mb-2 text-sm'>{t('system.retry.maxSingleChannelRetries.description')}</div>
                <Input
                  id='max-single-channel-retries'
                  type='number'
                  min='0'
                  max='5'
                  value={formData.maxSingleChannelRetries}
                  onChange={(e) => handleInputChange('maxSingleChannelRetries', parseInt(e.target.value) || 0)}
                  className='w-32'
                />
              </div>

              {/* Retry Delay */}
              <div className='space-y-2'>
                <Label htmlFor='retry-delay'>{t('system.retry.retryDelayMs.label')}</Label>
                <div className='text-muted-foreground mb-2 text-sm'>{t('system.retry.retryDelayMs.description')}</div>
                <div className='flex items-center space-x-2'>
                  <Input
                    id='retry-delay'
                    type='number'
                    min='100'
                    max='10000'
                    step='100'
                    value={formData.retryDelayMs}
                    onChange={(e) => handleInputChange('retryDelayMs', parseInt(e.target.value) || 1000)}
                    className='w-32'
                  />
                  <span className='text-muted-foreground text-sm'>ms</span>
                </div>
              </div>

              {/* Response Timeouts */}
              <div className='grid gap-4 md:grid-cols-2'>
                <div className='space-y-2'>
                  <Label htmlFor='stream-first-event-timeout'>{t('system.retry.streamFirstEventTimeoutSeconds.label')}</Label>
                  <div className='text-muted-foreground mb-2 text-sm'>{t('system.retry.streamFirstEventTimeoutSeconds.description')}</div>
                  <div className='flex items-center space-x-2'>
                    <Input
                      id='stream-first-event-timeout'
                      type='number'
                      min='0'
                      max='600'
                      value={formData.streamFirstEventTimeoutSeconds}
                      onChange={(e) => handleInputChange('streamFirstEventTimeoutSeconds', parseInt(e.target.value) || 0)}
                      className='w-32'
                    />
                    <span className='text-muted-foreground text-sm'>s</span>
                  </div>
                </div>

                <div className='space-y-2'>
                  <Label htmlFor='non-stream-response-timeout'>{t('system.retry.nonStreamResponseTimeoutSeconds.label')}</Label>
                  <div className='text-muted-foreground mb-2 text-sm'>{t('system.retry.nonStreamResponseTimeoutSeconds.description')}</div>
                  <div className='flex items-center space-x-2'>
                    <Input
                      id='non-stream-response-timeout'
                      type='number'
                      min='0'
                      max='600'
                      value={formData.nonStreamResponseTimeoutSeconds}
                      onChange={(e) => handleInputChange('nonStreamResponseTimeoutSeconds', parseInt(e.target.value) || 0)}
                      className='w-32'
                    />
                    <span className='text-muted-foreground text-sm'>s</span>
                  </div>
                </div>
              </div>

              {/* Empty Response Detection */}
              <div className='flex items-center justify-between'>
                <div className='space-y-0.5'>
                  <Label htmlFor='empty-response-detection' className='text-base'>
                    {t('system.retry.emptyResponseDetection.label')}
                  </Label>
                  <div className='text-muted-foreground text-sm'>{t('system.retry.emptyResponseDetection.description')}</div>
                </div>
                <Switch
                  id='empty-response-detection'
                  checked={formData.emptyResponseDetection || false}
                  onCheckedChange={(checked) => handleInputChange('emptyResponseDetection', checked)}
                />
              </div>

              {formData.emptyResponseDetection && (
                <div className='bg-muted/30 ml-4 space-y-2 rounded-md border p-4'>
                  <Label htmlFor='empty-response-text-patterns'>{t('system.retry.emptyResponseTextPatterns.label')}</Label>
                  <div className='text-muted-foreground text-sm'>{t('system.retry.emptyResponseTextPatterns.description')}</div>
                  <Textarea
                    id='empty-response-text-patterns'
                    value={(formData.emptyResponseTextPatterns || []).join('\n')}
                    onChange={(e) => handleEmptyResponseTextPatternsChange(e.target.value)}
                    placeholder={t('system.retry.emptyResponseTextPatterns.placeholder')}
                    className='min-h-24'
                  />
                  <div className='text-muted-foreground text-xs'>{t('system.retry.emptyResponseTextPatterns.help')}</div>
                </div>
              )}

              <Separator />

              {/* Auto Disable Channel */}
              <div className='space-y-4'>
                <div className='rounded-md border p-3'>
                  <Label className='text-base'>{t('system.retry.failurePolicy.label')}</Label>
                  <div className='text-muted-foreground mt-1 text-sm'>{t('system.retry.failurePolicy.description')}</div>
                  <div className='text-muted-foreground mt-2 text-xs'>
                    {t('system.retry.failurePolicy.summary', {
                      keyCount: formData.failurePolicy?.keyProfiles?.length || 0,
                      channelCount: formData.failurePolicy?.channelProfiles?.length || 0,
                    })}
                  </div>
                  <div className='mt-4 grid gap-4 lg:grid-cols-2'>
                    <FailurePolicyProfilesEditor
                      target='key'
                      profiles={formData.failurePolicy?.keyProfiles || []}
                      onChange={(profiles) => handleFailureProfilesChange('key', profiles)}
                    />
                    <FailurePolicyProfilesEditor
                      target='channel'
                      profiles={formData.failurePolicy?.channelProfiles || []}
                      onChange={(profiles) => handleFailureProfilesChange('channel', profiles)}
                    />
                  </div>
                </div>

                <div className='flex items-center justify-between'>
                  <div className='space-y-0.5'>
                    <Label htmlFor='auto-disable-channel' className='text-base'>
                      {t('system.retry.autoDisableChannel.label')}
                    </Label>
                    <div className='text-muted-foreground text-sm'>{t('system.retry.autoDisableChannel.description')}</div>
                  </div>
                  <Switch
                    id='auto-disable-channel'
                    checked={formData.autoDisableChannel?.enabled || false}
                    onCheckedChange={(checked) => handleAutoDisableChannelChange('enabled', checked)}
                  />
                </div>

                {formData.autoDisableChannel?.enabled && (
                  <div className='space-y-3'>
                    <div className='flex items-center justify-between'>
                      <Label className='text-sm font-medium'>{t('system.retry.autoDisableChannel.statuses.label')}</Label>
                      <Button type='button' variant='outline' size='sm' onClick={addStatus}>
                        <Plus className='mr-1 h-4 w-4' />
                        {t('system.retry.autoDisableChannel.statuses.add')}
                      </Button>
                    </div>

                    {formData.autoDisableChannel?.statuses && formData.autoDisableChannel.statuses.length > 0 ? (
                      <div className='space-y-2'>
                        {formData.autoDisableChannel.statuses.map((statusItem, index) => (
                          <div key={index} className='flex items-center space-x-2'>
                            <Input
                              type='number'
                              placeholder={t('system.retry.autoDisableChannel.statuses.statusPlaceholder')}
                              value={statusItem.status}
                              onChange={(e) => handleStatusChange(index, 'status', parseInt(e.target.value) || 0)}
                              className='w-24'
                              min='400'
                              max='599'
                            />
                            <Input
                              type='number'
                              placeholder={t('system.retry.autoDisableChannel.statuses.timesPlaceholder')}
                              value={statusItem.times}
                              onChange={(e) => handleStatusChange(index, 'times', parseInt(e.target.value) || 0)}
                              className='w-24'
                              min='1'
                              max='100'
                            />
                            <span className='text-muted-foreground text-sm'>{t('system.retry.autoDisableChannel.statuses.times')}</span>
                            <Button type='button' variant='ghost' size='icon' onClick={() => removeStatus(index)}>
                              <Trash2 className='h-4 w-4' />
                            </Button>
                          </div>
                        ))}
                      </div>
                    ) : (
                      <div className='text-muted-foreground text-sm'>{t('system.retry.autoDisableChannel.statuses.empty')}</div>
                    )}
                  </div>
                )}
              </div>
            </div>
          )}

          <Separator />

          {/* Submit Button */}
          <div className='flex justify-end'>
            <Button type='submit' disabled={updateRetryPolicy.isPending} className='min-w-24'>
              {updateRetryPolicy.isPending ? <Loader2 className='h-4 w-4 animate-spin' /> : t('common.buttons.save')}
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}

function FailurePolicyProfilesEditor({
  target,
  profiles,
  onChange,
}: {
  target: FailurePolicyTarget;
  profiles: FailurePolicyProfile[];
  onChange: (profiles: FailurePolicyProfile[]) => void;
}) {
  const { t } = useTranslation();
  const actionChoices = target === 'key' ? KEY_FAILURE_ACTIONS : CHANNEL_FAILURE_ACTIONS;

  const updateProfile = (index: number, patch: Partial<FailurePolicyProfile>) => {
    onChange(profiles.map((profile, profileIndex) => (profileIndex === index ? { ...profile, ...patch } : profile)));
  };

  const updateConditions = (index: number, patch: Partial<NonNullable<FailurePolicyProfile['conditions']>>) => {
    const profile = profiles[index];
    updateProfile(index, {
      conditions: {
        minFailureCount: profile.conditions?.minFailureCount ?? null,
        statusCodes: profile.conditions?.statusCodes ?? [],
        available: profile.conditions?.available ?? null,
        balanceLTE: profile.conditions?.balanceLTE ?? null,
        reasonContains: profile.conditions?.reasonContains ?? null,
        allCheckedKeysFailed: profile.conditions?.allCheckedKeysFailed ?? null,
        expr: profile.conditions?.expr ?? null,
        ...patch,
      },
    });
  };

  const updateAction = (
    profileIndex: number,
    actionIndex: number,
    patch: Partial<NonNullable<FailurePolicyProfile['actions']>[number]>
  ) => {
    const actions = profiles[profileIndex].actions || [];
    updateProfile(profileIndex, {
      actions: actions.map((action, index) => (index === actionIndex ? { ...action, ...patch } : action)),
    });
  };

  const addProfile = () => onChange([...profiles, createDefaultProfile(profiles.length, target)]);
  const removeProfile = (index: number) => onChange(profiles.filter((_, profileIndex) => profileIndex !== index));

  return (
    <div className='space-y-3 rounded-md border p-3'>
      <div className='flex items-center justify-between gap-3'>
        <div>
          <Label className='text-sm font-medium'>
            {t(`system.retry.failurePolicy.${target === 'key' ? 'keyProfiles' : 'channelProfiles'}.title`)}
          </Label>
          <div className='text-muted-foreground text-xs'>
            {t(`system.retry.failurePolicy.${target === 'key' ? 'keyProfiles' : 'channelProfiles'}.description`)}
          </div>
        </div>
        <Button type='button' variant='outline' size='sm' onClick={addProfile}>
          <Plus className='mr-1 h-4 w-4' />
          {t('system.retry.failurePolicy.profiles.add')}
        </Button>
      </div>

      {profiles.length === 0 ? (
        <div className='text-muted-foreground rounded-md border border-dashed p-3 text-sm'>
          {t('system.retry.failurePolicy.profiles.empty')}
        </div>
      ) : (
        profiles.map((profile, profileIndex) => (
          <div key={profile.id || profileIndex} className='space-y-3 rounded-md border p-3'>
            <div className='flex items-start justify-between gap-2'>
              <div className='grid flex-1 gap-3 md:grid-cols-[1fr_auto]'>
                <div className='space-y-1'>
                  <Label>{t('system.retry.failurePolicy.profiles.fields.name')}</Label>
                  <Input value={profile.name || ''} onChange={(event) => updateProfile(profileIndex, { name: event.target.value })} />
                </div>
                <div className='flex items-end gap-2'>
                  <div className='flex items-center gap-2 pb-2'>
                    <Switch
                      checked={profile.enabled ?? true}
                      onCheckedChange={(checked) => updateProfile(profileIndex, { enabled: checked })}
                    />
                    <Label>{t(`system.retry.failurePolicy.profiles.${profile.enabled === false ? 'disabled' : 'enabled'}`)}</Label>
                  </div>
                  <Button type='button' variant='ghost' size='icon' onClick={() => removeProfile(profileIndex)}>
                    <Trash2 className='h-4 w-4' />
                  </Button>
                </div>
              </div>
            </div>

            <div className='space-y-2'>
              <Label>{t('system.retry.failurePolicy.profiles.fields.sources')}</Label>
              <div className='grid gap-2'>
                {FAILURE_EVENT_SOURCES.map((source) => {
                  const currentSources =
                    profile.sources && profile.sources.length > 0
                      ? profile.sources
                      : target === 'key'
                        ? ['request_failure']
                        : ['request_failure'];
                  const selected = currentSources.includes(source);
                  return (
                    <label key={source} className='flex items-center gap-2 text-sm'>
                      <Checkbox
                        checked={selected}
                        onCheckedChange={(checked) => {
                          updateProfile(profileIndex, {
                            sources: checked
                              ? [...currentSources.filter((item) => item !== source), source]
                              : currentSources.length <= 1
                                ? currentSources
                                : currentSources.filter((item) => item !== source),
                          });
                        }}
                      />
                      {t(`system.retry.failurePolicy.sources.${source}`)}
                    </label>
                  );
                })}
              </div>
            </div>

            <div className='grid gap-3 md:grid-cols-3'>
              <div className='space-y-1'>
                <Label>{t('system.retry.failurePolicy.profiles.fields.minFailureCount')}</Label>
                <Input
                  type='number'
                  min='1'
                  max='100'
                  value={profile.conditions?.minFailureCount ?? ''}
                  onChange={(event) =>
                    updateConditions(profileIndex, {
                      minFailureCount: event.target.value === '' ? null : parseInt(event.target.value) || null,
                    })
                  }
                />
              </div>
              <div className='space-y-1'>
                <Label>{t('system.retry.failurePolicy.profiles.fields.statusCodes')}</Label>
                <Input
                  placeholder='429, 500'
                  value={(profile.conditions?.statusCodes || []).join(', ')}
                  onChange={(event) => updateConditions(profileIndex, { statusCodes: parseStatusList(event.target.value) })}
                />
              </div>
              <div className='space-y-1'>
                <Label>{t('system.retry.failurePolicy.profiles.fields.availability')}</Label>
                <Select
                  value={profile.conditions?.available == null ? 'any' : profile.conditions.available ? 'available' : 'unavailable'}
                  onValueChange={(value) =>
                    updateConditions(profileIndex, {
                      available: value === 'any' ? null : value === 'available',
                    })
                  }
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value='any'>{t('system.retry.failurePolicy.profiles.availability.any')}</SelectItem>
                    <SelectItem value='available'>{t('system.retry.failurePolicy.profiles.availability.available')}</SelectItem>
                    <SelectItem value='unavailable'>{t('system.retry.failurePolicy.profiles.availability.unavailable')}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className='space-y-1'>
                <Label>{t('system.retry.failurePolicy.profiles.fields.balanceLTE')}</Label>
                <Input
                  type='number'
                  value={profile.conditions?.balanceLTE ?? ''}
                  onChange={(event) =>
                    updateConditions(profileIndex, {
                      balanceLTE: event.target.value === '' ? null : Number(event.target.value),
                    })
                  }
                />
              </div>
              <div className='space-y-1'>
                <Label>{t('system.retry.failurePolicy.profiles.fields.reasonContains')}</Label>
                <Input
                  value={profile.conditions?.reasonContains ?? ''}
                  onChange={(event) => updateConditions(profileIndex, { reasonContains: event.target.value || null })}
                />
              </div>
              <div className='flex items-center justify-between gap-2 rounded-md border p-3'>
                <Label>{t('system.retry.failurePolicy.profiles.fields.allCheckedKeysFailed')}</Label>
                <Switch
                  checked={profile.conditions?.allCheckedKeysFailed === true}
                  onCheckedChange={(checked) => updateConditions(profileIndex, { allCheckedKeysFailed: checked || null })}
                />
              </div>
              <div className='space-y-1 md:col-span-3'>
                <Label>{t('system.retry.failurePolicy.profiles.fields.expr')}</Label>
                <Textarea
                  rows={2}
                  value={profile.conditions?.expr ?? ''}
                  onChange={(event) => updateConditions(profileIndex, { expr: event.target.value || null })}
                />
              </div>
            </div>

            <div className='space-y-2'>
              <div className='flex items-center justify-between'>
                <Label>{t('system.retry.failurePolicy.actions.title')}</Label>
                <Button
                  type='button'
                  variant='outline'
                  size='sm'
                  onClick={() => updateProfile(profileIndex, { actions: [...(profile.actions || []), createDefaultAction(target)] })}
                >
                  <Plus className='mr-1 h-4 w-4' />
                  {t('system.retry.failurePolicy.actions.add')}
                </Button>
              </div>
              {(profile.actions || []).map((action, actionIndex) => (
                <div key={actionIndex} className='grid gap-3 rounded-md border p-3 md:grid-cols-4'>
                  <div className='space-y-1'>
                    <Label>{t('system.retry.failurePolicy.actions.type')}</Label>
                    <Select
                      value={actionChoices.includes(action.type) ? action.type : 'report_only'}
                      onValueChange={(value) =>
                        updateAction(profileIndex, actionIndex, {
                          type: value as FailurePolicyActionType,
                          backoff:
                            value === 'backoff_key'
                              ? {
                                  mode: action.backoff?.mode ?? 'fixed',
                                  intervalMinutes: action.backoff?.intervalMinutes ?? 30,
                                  maxIntervalMinutes: action.backoff?.maxIntervalMinutes ?? 240,
                                  multiplier: action.backoff?.multiplier ?? 2,
                                }
                              : null,
                        })
                      }
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {actionChoices.map((choice) => (
                          <SelectItem key={choice} value={choice}>
                            {t(`system.retry.failurePolicy.actions.${choice}`)}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  {action.type === 'backoff_key' ? (
                    <>
                      <div className='space-y-1'>
                        <Label>{t('system.retry.failurePolicy.actions.intervalMinutes')}</Label>
                        <Input
                          type='number'
                          min='1'
                          max='10080'
                          value={action.backoff?.intervalMinutes ?? 30}
                          onChange={(event) =>
                            updateAction(profileIndex, actionIndex, {
                              backoff: { ...action.backoff, intervalMinutes: parseInt(event.target.value) || 30 },
                            })
                          }
                        />
                      </div>
                      <div className='space-y-1'>
                        <Label>{t('system.retry.failurePolicy.actions.maxIntervalMinutes')}</Label>
                        <Input
                          type='number'
                          min='1'
                          max='10080'
                          value={action.backoff?.maxIntervalMinutes ?? 240}
                          onChange={(event) =>
                            updateAction(profileIndex, actionIndex, {
                              backoff: { ...action.backoff, maxIntervalMinutes: parseInt(event.target.value) || 240 },
                            })
                          }
                        />
                      </div>
                      <div className='space-y-1'>
                        <Label>{t('system.retry.failurePolicy.actions.multiplier')}</Label>
                        <Input
                          type='number'
                          min='1'
                          max='20'
                          step='0.1'
                          value={action.backoff?.multiplier ?? 2}
                          onChange={(event) =>
                            updateAction(profileIndex, actionIndex, {
                              backoff: { ...action.backoff, multiplier: Number(event.target.value) || 2 },
                            })
                          }
                        />
                      </div>
                    </>
                  ) : null}
                  <div className='flex items-end justify-end md:col-start-4'>
                    <Button
                      type='button'
                      variant='ghost'
                      size='icon'
                      onClick={() =>
                        updateProfile(profileIndex, {
                          actions: (profile.actions || []).filter((_, index) => index !== actionIndex),
                        })
                      }
                      disabled={(profile.actions || []).length <= 1}
                    >
                      <Trash2 className='h-4 w-4' />
                    </Button>
                  </div>
                </div>
              ))}
            </div>
          </div>
        ))
      )}
    </div>
  );
}
