'use client';

import { useState } from 'react';
import { z } from 'zod';
import { useFieldArray, type UseFormReturn } from 'react-hook-form';
import { IconChevronDown, IconChevronUp, IconPlus, IconTrash } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Checkbox } from '@/components/ui/checkbox';
import { FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import { Textarea } from '@/components/ui/textarea';
import {
  type ChannelFailurePolicy,
  type ChannelFailurePolicyMode,
  type ChannelKeyHealthCheck,
  type FailurePolicyActionType,
  type FailurePolicyEventSource,
} from '../data/schema';

type FailurePolicyTarget = 'key' | 'channel';
type AvailabilityConditionMode = 'any' | 'available' | 'unavailable';

const nullableIntField = (min: number, max: number) =>
  z.preprocess((value) => (value === '' || value == null ? null : value), z.coerce.number().int().min(min).max(max).nullable());
const nullableNumberField = z.preprocess((value) => (value === '' || value == null ? null : value), z.coerce.number().nullable());

export const failureProfileFormSchema = z.object({
  id: z.string().min(1),
  name: z.string().min(1),
  enabled: z.boolean(),
  sources: z
    .array(
      z.enum([
        'request_failure',
        'scheduled_health_check',
        'manual_health_check',
        'scheduled_health_check_failure',
        'manual_health_check_failure',
        'scheduled_balance_probe',
        'scheduled_balance_probe_failure',
        'manual_balance_probe',
        'manual_balance_probe_failure',
      ])
    )
    .min(1),
  minFailureCount: nullableIntField(1, 100),
  statusCodes: z.string().optional(),
  availability: z.enum(['any', 'available', 'unavailable']),
  balanceLTE: nullableNumberField,
  reasonContains: z.string().optional(),
  allCheckedKeysFailed: z.boolean(),
  expr: z.string().optional(),
  actions: z.array(
    z.object({
      type: z.enum([
        'report_only',
        'backoff_key',
        'disable_key',
        'archive_key',
        'delete_key',
        'disable_channel',
        'enable_key',
        'restore_key',
      ]),
      backoffMode: z.enum(['fixed', 'exponential']),
      intervalMinutes: z.coerce.number().int().min(1).max(10080),
      maxIntervalMinutes: z.coerce.number().int().min(1).max(10080),
      multiplier: z.coerce.number().min(1).max(20),
    })
  ),
});

export const failurePolicyFormSchema = z.object({
  mode: z.enum(['inherit', 'override', 'merge', 'disabled']),
  keyProfiles: z.array(failureProfileFormSchema),
  channelProfiles: z.array(failureProfileFormSchema),
});

export type FailurePolicyFormValue = z.output<typeof failurePolicyFormSchema>;
type FailureProfileFormValue = z.output<typeof failureProfileFormSchema>;
type FailureActionFormValue = FailureProfileFormValue['actions'][number];

const FAILURE_POLICY_MODES: ChannelFailurePolicyMode[] = ['inherit', 'override', 'merge', 'disabled'];
const FAILURE_EVENT_SOURCES: FailurePolicyEventSource[] = [
  'request_failure',
  'scheduled_health_check',
  'manual_health_check',
  'scheduled_health_check_failure',
  'manual_health_check_failure',
  'scheduled_balance_probe',
  'scheduled_balance_probe_failure',
  'manual_balance_probe',
  'manual_balance_probe_failure',
];
const KEY_FAILURE_ACTIONS: FailurePolicyActionType[] = [
  'report_only',
  'backoff_key',
  'disable_key',
  'archive_key',
  'delete_key',
  'enable_key',
  'restore_key',
];
const CHANNEL_FAILURE_ACTIONS: FailurePolicyActionType[] = ['report_only', 'disable_channel'];

function positiveOrDefault(value: number | null | undefined, fallback: number): number {
  return typeof value === 'number' && value > 0 ? value : fallback;
}

function createDefaultProfile(index: number, target: FailurePolicyTarget): FailureProfileFormValue {
  return {
    id: `${target}-policy-${Date.now()}-${index + 1}`,
    name: `${target === 'key' ? 'Key' : 'Channel'} policy ${index + 1}`,
    enabled: true,
    sources:
      target === 'key' ? ['request_failure', 'scheduled_health_check_failure', 'scheduled_balance_probe_failure'] : ['request_failure'],
    minFailureCount: 3,
    statusCodes: '',
    availability: 'any',
    balanceLTE: null,
    reasonContains: '',
    allCheckedKeysFailed: false,
    expr: '',
    actions: [
      {
        type: 'report_only',
        backoffMode: 'fixed',
        intervalMinutes: 30,
        maxIntervalMinutes: 240,
        multiplier: 2,
      },
    ],
  };
}

function createDefaultAction(target: FailurePolicyTarget): FailureActionFormValue {
  return {
    type: target === 'key' ? 'report_only' : 'report_only',
    backoffMode: 'fixed',
    intervalMinutes: 30,
    maxIntervalMinutes: 240,
    multiplier: 2,
  };
}

function parseStatusList(input: string): number[] {
  return input
    .split(',')
    .map((item) => Number(item.trim()))
    .filter((item) => Number.isInteger(item) && item >= 100 && item <= 599);
}

function toProfileFormValue(
  profile: NonNullable<ChannelFailurePolicy['keyProfiles']>[number],
  index: number,
  target: FailurePolicyTarget,
  sourcesFallback: FailurePolicyEventSource[]
): FailureProfileFormValue {
  const allowedActions = target === 'key' ? KEY_FAILURE_ACTIONS : CHANNEL_FAILURE_ACTIONS;
  const actions =
    profile.actions
      ?.filter((action) => allowedActions.includes(action.type))
      .map((action) => ({
        type: action.type,
        backoffMode: action.backoff?.mode ?? 'fixed',
        intervalMinutes: positiveOrDefault(action.backoff?.intervalMinutes, 30),
        maxIntervalMinutes: positiveOrDefault(action.backoff?.maxIntervalMinutes, 240),
        multiplier: positiveOrDefault(action.backoff?.multiplier, 2),
      })) ?? [];

  return {
    id: profile.id || `${target}-policy-${index + 1}`,
    name: profile.name || `${target === 'key' ? 'Key' : 'Channel'} policy ${index + 1}`,
    enabled: profile.enabled ?? true,
    sources: profile.sources && profile.sources.length > 0 ? profile.sources : sourcesFallback,
    minFailureCount: profile.conditions?.minFailureCount ?? null,
    statusCodes: (profile.conditions?.statusCodes ?? []).join(', '),
    availability: profile.conditions?.available == null ? 'any' : profile.conditions.available ? 'available' : 'unavailable',
    balanceLTE: profile.conditions?.balanceLTE ?? null,
    reasonContains: profile.conditions?.reasonContains ?? '',
    allCheckedKeysFailed: profile.conditions?.allCheckedKeysFailed ?? false,
    expr: profile.conditions?.expr ?? '',
    actions: actions.length > 0 ? actions : [createDefaultAction(target)],
  };
}

export function failurePolicyValuesFromStored(policy?: ChannelFailurePolicy | null): FailurePolicyFormValue {
  return {
    mode: policy?.mode ?? 'inherit',
    keyProfiles: (policy?.keyProfiles ?? []).map((profile, index) =>
      toProfileFormValue(profile, index, 'key', ['request_failure', 'scheduled_health_check_failure'])
    ),
    channelProfiles: (policy?.channelProfiles ?? []).map((profile, index) =>
      toProfileFormValue(profile, index, 'channel', ['request_failure'])
    ),
  };
}

function mapLegacyHealthActionType(type: string): FailurePolicyActionType | null {
  if (type === 'backoff') return 'backoff_key';
  if (type === 'report_only') return 'report_only';
  if (type === 'disable_key' || type === 'archive_key' || type === 'delete_key' || type === 'disable_channel') return type;
  return null;
}

export function failurePolicyValuesFromLegacyHealth(health?: ChannelKeyHealthCheck | null): FailurePolicyFormValue {
  const keyProfiles: FailureProfileFormValue[] = [];
  const channelProfiles: FailureProfileFormValue[] = [];
  const legacySources: FailurePolicyEventSource[] = ['scheduled_health_check_failure', 'manual_health_check_failure'];

  (health?.policies ?? []).forEach((policy, index) => {
    const legacyProfile = {
      id: policy.id,
      name: policy.name,
      enabled: policy.enabled,
      sources: legacySources,
      conditions: policy.conditions,
      actions: (policy.actions ?? [])
        .map((action) => {
          const type = mapLegacyHealthActionType(action.type);
          return type
            ? {
                type,
                backoff: type === 'backoff_key' ? action.backoff : null,
              }
            : null;
        })
        .filter((action): action is NonNullable<typeof action> => action != null),
    };

    const keyProfile = toProfileFormValue(legacyProfile, index, 'key', legacySources);

    if (keyProfile.actions.some((action) => KEY_FAILURE_ACTIONS.includes(action.type))) {
      keyProfiles.push({
        ...keyProfile,
        actions: keyProfile.actions.filter((action) => KEY_FAILURE_ACTIONS.includes(action.type)),
      });
    }

    if ((policy.actions ?? []).some((action) => action.type === 'disable_channel') || policy.conditions?.allCheckedKeysFailed) {
      const channelProfile = toProfileFormValue(legacyProfile, index, 'channel', legacySources);
      channelProfiles.push({
        ...channelProfile,
        id: `${channelProfile.id}-channel`,
        name: `${channelProfile.name} (channel)`,
        actions: channelProfile.actions.filter((action) => CHANNEL_FAILURE_ACTIONS.includes(action.type)),
      });
    }
  });

  if (health?.failureAction && health.failureAction !== 'report_only') {
    keyProfiles.push({
      ...createDefaultProfile(keyProfiles.length, 'key'),
      id: 'legacy-health-threshold',
      name: 'Legacy health-check threshold',
      sources: legacySources,
      minFailureCount: health.failureThreshold ?? 3,
      actions: [
        {
          ...createDefaultAction('key'),
          type: health.failureAction === 'disable' ? 'disable_key' : health.failureAction === 'archive' ? 'archive_key' : 'delete_key',
        },
      ],
    });
  }

  return {
    mode: 'merge',
    keyProfiles,
    channelProfiles: channelProfiles.filter((profile) => profile.actions.length > 0),
  };
}

export function failurePolicyFromValues(values: { failurePolicy: FailurePolicyFormValue }): ChannelFailurePolicy {
  const toStoredProfile = (policy: FailureProfileFormValue, target: FailurePolicyTarget) => ({
    id: policy.id,
    name: policy.name,
    enabled: policy.enabled,
    sources: policy.sources,
    conditions: {
      minFailureCount: policy.minFailureCount ?? null,
      statusCodes: parseStatusList(policy.statusCodes ?? ''),
      available: policy.availability === 'any' ? null : policy.availability === 'available',
      balanceLTE: policy.balanceLTE ?? null,
      reasonContains: policy.reasonContains?.trim() || null,
      allCheckedKeysFailed: policy.allCheckedKeysFailed || null,
      expr: policy.expr?.trim() || null,
    },
    actions: policy.actions
      .filter((action) => (target === 'key' ? KEY_FAILURE_ACTIONS : CHANNEL_FAILURE_ACTIONS).includes(action.type))
      .map((action) => ({
        type: action.type,
        backoff:
          action.type === 'backoff_key'
            ? {
                mode: action.backoffMode,
                intervalMinutes: action.intervalMinutes,
                maxIntervalMinutes: action.maxIntervalMinutes,
                multiplier: action.multiplier,
              }
            : null,
      })),
  });

  return {
    mode: values.failurePolicy.mode,
    keyProfiles: values.failurePolicy.keyProfiles.map((profile) => toStoredProfile(profile, 'key')),
    channelProfiles: values.failurePolicy.channelProfiles.map((profile) => toStoredProfile(profile, 'channel')),
  };
}

export function FailurePolicyEditor<TFormValues extends { failurePolicy: FailurePolicyFormValue }>({
  form,
  disabled,
}: {
  form: UseFormReturn<TFormValues, unknown, TFormValues>;
  disabled: boolean;
}) {
  const { t } = useTranslation();

  return (
    <div className='space-y-5'>
      <FormField
        control={form.control}
        name={'failurePolicy.mode' as never}
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('channels.dialogs.keys.failureStrategy.mode.label')}</FormLabel>
            <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
              <FormControl>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
              </FormControl>
              <SelectContent>
                {FAILURE_POLICY_MODES.map((mode) => (
                  <SelectItem key={mode} value={mode}>
                    {t(`channels.dialogs.keys.failureStrategy.modes.${mode}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <FormDescription>{t('channels.dialogs.keys.failureStrategy.mode.description')}</FormDescription>
          </FormItem>
        )}
      />

      <div className='grid gap-4 lg:grid-cols-2'>
        <FailurePolicyProfilesEditor
          form={form}
          disabled={disabled}
          target='key'
          name='failurePolicy.keyProfiles'
          title={t('channels.dialogs.keys.failureStrategy.keyProfiles.title')}
          description={t('channels.dialogs.keys.failureStrategy.keyProfiles.description')}
        />
        <FailurePolicyProfilesEditor
          form={form}
          disabled={disabled}
          target='channel'
          name='failurePolicy.channelProfiles'
          title={t('channels.dialogs.keys.failureStrategy.channelProfiles.title')}
          description={t('channels.dialogs.keys.failureStrategy.channelProfiles.description')}
        />
      </div>
    </div>
  );
}

function FailurePolicyProfilesEditor<TFormValues extends { failurePolicy: FailurePolicyFormValue }>({
  form,
  disabled,
  target,
  name,
  title,
  description,
}: {
  form: UseFormReturn<TFormValues, unknown, TFormValues>;
  disabled: boolean;
  target: FailurePolicyTarget;
  name: 'failurePolicy.keyProfiles' | 'failurePolicy.channelProfiles';
  title: string;
  description: string;
}) {
  const { t } = useTranslation();
  const [expandedPolicies, setExpandedPolicies] = useState<Set<string>>(new Set());
  const {
    fields: policyFields,
    append: appendPolicy,
    remove: removePolicy,
  } = useFieldArray({
    control: form.control,
    name: name as never,
  });

  const addPolicy = () => {
    const policy = createDefaultProfile(policyFields.length, target);
    appendPolicy(policy as never);
    setExpandedPolicies((prev) => new Set(prev).add(policy.id));
  };

  const togglePolicy = (id: string) => {
    setExpandedPolicies((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  return (
    <div className='space-y-3 rounded-lg border p-3'>
      <div className='flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between'>
        <div>
          <h4 className='text-sm font-medium'>{title}</h4>
          <p className='text-muted-foreground text-xs'>{description}</p>
        </div>
        <Button type='button' variant='outline' size='sm' onClick={addPolicy} disabled={disabled}>
          <IconPlus className='mr-2 h-4 w-4' />
          {t('channels.dialogs.keys.failureStrategy.profiles.add')}
        </Button>
      </div>

      {policyFields.length === 0 ? (
        <div className='text-muted-foreground rounded-lg border border-dashed p-4 text-sm'>
          {t('channels.dialogs.keys.failureStrategy.profiles.empty')}
        </div>
      ) : (
        policyFields.map((policyField, policyIndex) => {
          const profilePath = `${name}.${policyIndex}` as const;
          const policyID = (form.watch(`${profilePath}.id` as never) as string) || policyField.id;
          const policyName =
            (form.watch(`${profilePath}.name` as never) as string) || t('channels.dialogs.keys.failureStrategy.profiles.unnamed');
          const policyEnabled = form.watch(`${profilePath}.enabled` as never) as boolean;
          const actions = (form.watch(`${profilePath}.actions` as never) as FailureActionFormValue[]) ?? [];
          const isExpanded = expandedPolicies.has(policyID);

          return (
            <div key={policyField.id} className='rounded-lg border'>
              <div className='flex flex-col gap-2 p-3 sm:flex-row sm:items-center sm:justify-between'>
                <button type='button' className='flex min-w-0 items-center gap-2 text-left' onClick={() => togglePolicy(policyID)}>
                  {isExpanded ? <IconChevronUp className='h-4 w-4' /> : <IconChevronDown className='h-4 w-4' />}
                  <div className='min-w-0'>
                    <div className='truncate text-sm font-medium'>{policyName}</div>
                    <div className='text-muted-foreground text-xs'>
                      {t('channels.dialogs.keys.failureStrategy.profiles.actionCount', { count: actions.length })}
                    </div>
                  </div>
                </button>
                <div className='flex items-center gap-2'>
                  <Badge variant={policyEnabled ? 'default' : 'outline'}>
                    {t(`channels.dialogs.keys.failureStrategy.profiles.${policyEnabled ? 'enabled' : 'disabled'}`)}
                  </Badge>
                  <FormField
                    control={form.control}
                    name={`${profilePath}.enabled` as never}
                    render={({ field }) => (
                      <FormItem>
                        <FormControl>
                          <Switch checked={field.value} onCheckedChange={field.onChange} disabled={disabled} />
                        </FormControl>
                      </FormItem>
                    )}
                  />
                  <Button type='button' variant='ghost' size='sm' onClick={() => removePolicy(policyIndex)} disabled={disabled}>
                    <IconTrash className='h-4 w-4' />
                  </Button>
                </div>
              </div>

              {isExpanded ? (
                <div className='space-y-4 border-t p-3'>
                  <FormField
                    control={form.control}
                    name={`${profilePath}.name` as never}
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.name')}</FormLabel>
                        <FormControl>
                          <Input {...field} disabled={disabled} />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />

                  <div className='space-y-2 rounded-lg border p-3'>
                    <div className='text-sm font-medium'>{t('channels.dialogs.keys.failureStrategy.profiles.fields.sources')}</div>
                    <div className='grid gap-2'>
                      {FAILURE_EVENT_SOURCES.map((source) => (
                        <FormField
                          key={source}
                          control={form.control}
                          name={`${profilePath}.sources` as never}
                          render={({ field }) => {
                            const selected = field.value?.includes(source) ?? false;
                            return (
                              <FormItem className='flex items-center gap-2 space-y-0'>
                                <FormControl>
                                  <Checkbox
                                    checked={selected}
                                    onCheckedChange={(checked) => {
                                      const current = field.value ?? [];
                                      field.onChange(
                                        checked
                                          ? [...current.filter((item: FailurePolicyEventSource) => item !== source), source]
                                          : current.length <= 1
                                            ? current
                                            : current.filter((item: FailurePolicyEventSource) => item !== source)
                                      );
                                    }}
                                    disabled={disabled}
                                  />
                                </FormControl>
                                <FormLabel className='text-sm font-normal'>
                                  {t(`channels.dialogs.keys.failureStrategy.sources.${source}`)}
                                </FormLabel>
                              </FormItem>
                            );
                          }}
                        />
                      ))}
                    </div>
                  </div>

                  <div className='grid gap-4 md:grid-cols-3'>
                    <FormField
                      control={form.control}
                      name={`${profilePath}.minFailureCount` as never}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.minFailureCount')}</FormLabel>
                          <FormControl>
                            <Input
                              type='number'
                              min={1}
                              max={100}
                              value={typeof field.value === 'number' || typeof field.value === 'string' ? field.value : ''}
                              onBlur={field.onBlur}
                              onChange={(event) => field.onChange(event.target.value)}
                              disabled={disabled}
                            />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`${profilePath}.statusCodes` as never}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.statusCodes')}</FormLabel>
                          <FormControl>
                            <Input placeholder='429, 500' {...field} disabled={disabled} />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`${profilePath}.availability` as never}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.availability')}</FormLabel>
                          <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                            <FormControl>
                              <SelectTrigger>
                                <SelectValue />
                              </SelectTrigger>
                            </FormControl>
                            <SelectContent>
                              {(['any', 'available', 'unavailable'] satisfies AvailabilityConditionMode[]).map((mode) => (
                                <SelectItem key={mode} value={mode}>
                                  {t(`channels.dialogs.keys.failureStrategy.profiles.availability.${mode}`)}
                                </SelectItem>
                              ))}
                            </SelectContent>
                          </Select>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`${profilePath}.balanceLTE` as never}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.balanceLTE')}</FormLabel>
                          <FormControl>
                            <Input
                              type='number'
                              value={typeof field.value === 'number' || typeof field.value === 'string' ? field.value : ''}
                              onBlur={field.onBlur}
                              onChange={(event) => field.onChange(event.target.value)}
                              disabled={disabled}
                            />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`${profilePath}.reasonContains` as never}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.reasonContains')}</FormLabel>
                          <FormControl>
                            <Input {...field} disabled={disabled} />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`${profilePath}.allCheckedKeysFailed` as never}
                      render={({ field }) => (
                        <FormItem className='flex flex-row items-center justify-between rounded-lg border p-3'>
                          <div className='space-y-0.5'>
                            <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.allCheckedKeysFailed')}</FormLabel>
                          </div>
                          <FormControl>
                            <Switch checked={field.value} onCheckedChange={field.onChange} disabled={disabled} />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`${profilePath}.expr` as never}
                      render={({ field }) => (
                        <FormItem className='md:col-span-3'>
                          <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.expr')}</FormLabel>
                          <FormControl>
                            <Textarea rows={2} {...field} disabled={disabled} />
                          </FormControl>
                          <FormDescription>{t('channels.dialogs.keys.failureStrategy.profiles.fields.exprDescription')}</FormDescription>
                        </FormItem>
                      )}
                    />
                  </div>

                  <PolicyActionsEditor form={form} profileName={name} profileIndex={policyIndex} target={target} disabled={disabled} />
                </div>
              ) : null}
            </div>
          );
        })
      )}
    </div>
  );
}

function PolicyActionsEditor<TFormValues extends { failurePolicy: FailurePolicyFormValue }>({
  form,
  profileName,
  profileIndex,
  target,
  disabled,
}: {
  form: UseFormReturn<TFormValues, unknown, TFormValues>;
  profileName: 'failurePolicy.keyProfiles' | 'failurePolicy.channelProfiles';
  profileIndex: number;
  target: FailurePolicyTarget;
  disabled: boolean;
}) {
  const { t } = useTranslation();
  const actionsName = `${profileName}.${profileIndex}.actions` as const;
  const actionChoices = target === 'key' ? KEY_FAILURE_ACTIONS : CHANNEL_FAILURE_ACTIONS;
  const {
    fields: actionFields,
    append: appendAction,
    remove: removeAction,
  } = useFieldArray({
    control: form.control,
    name: actionsName as never,
  });

  return (
    <div className='space-y-3'>
      <div className='flex items-center justify-between'>
        <h5 className='text-sm font-medium'>{t('channels.dialogs.keys.failureStrategy.actions.title')}</h5>
        <Button
          type='button'
          variant='outline'
          size='sm'
          onClick={() => appendAction(createDefaultAction(target) as never)}
          disabled={disabled}
        >
          <IconPlus className='mr-2 h-4 w-4' />
          {t('channels.dialogs.keys.failureStrategy.actions.add')}
        </Button>
      </div>
      {actionFields.map((actionField, actionIndex) => {
        const actionPath = `${actionsName}.${actionIndex}` as const;
        const actionType = form.watch(`${actionPath}.type` as never) as FailurePolicyActionType;
        return (
          <div key={actionField.id} className='grid gap-3 rounded-lg border p-3 md:grid-cols-4'>
            <FormField
              control={form.control}
              name={`${actionPath}.type` as never}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('channels.dialogs.keys.failureStrategy.actions.type')}</FormLabel>
                  <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                    <FormControl>
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                    </FormControl>
                    <SelectContent>
                      {actionChoices.map((action) => (
                        <SelectItem key={action} value={action}>
                          {t(`channels.dialogs.keys.failureStrategy.actions.${action}`)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </FormItem>
              )}
            />
            {actionType === 'backoff_key' ? (
              <>
                <FormField
                  control={form.control}
                  name={`${actionPath}.backoffMode` as never}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.keys.failureStrategy.actions.backoffMode')}</FormLabel>
                      <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                        <FormControl>
                          <SelectTrigger>
                            <SelectValue />
                          </SelectTrigger>
                        </FormControl>
                        <SelectContent>
                          <SelectItem value='fixed'>{t('channels.dialogs.keys.failureStrategy.actions.fixed')}</SelectItem>
                          <SelectItem value='exponential'>{t('channels.dialogs.keys.failureStrategy.actions.exponential')}</SelectItem>
                        </SelectContent>
                      </Select>
                    </FormItem>
                  )}
                />
                <NumberActionField
                  form={form}
                  name={`${actionPath}.intervalMinutes`}
                  label={t('channels.dialogs.keys.failureStrategy.actions.intervalMinutes')}
                  disabled={disabled}
                />
                <NumberActionField
                  form={form}
                  name={`${actionPath}.maxIntervalMinutes`}
                  label={t('channels.dialogs.keys.failureStrategy.actions.maxIntervalMinutes')}
                  disabled={disabled}
                />
                <NumberActionField
                  form={form}
                  name={`${actionPath}.multiplier`}
                  label={t('channels.dialogs.keys.failureStrategy.actions.multiplier')}
                  disabled={disabled}
                  step='0.1'
                  max={20}
                />
              </>
            ) : null}
            <div className='flex items-end justify-end md:col-start-4'>
              <Button
                type='button'
                variant='ghost'
                size='sm'
                onClick={() => removeAction(actionIndex)}
                disabled={disabled || actionFields.length <= 1}
              >
                <IconTrash className='h-4 w-4' />
              </Button>
            </div>
          </div>
        );
      })}
    </div>
  );
}

function NumberActionField<TFormValues extends { failurePolicy: FailurePolicyFormValue }>({
  form,
  name,
  label,
  disabled,
  min = 1,
  max = 10080,
  step,
}: {
  form: UseFormReturn<TFormValues, unknown, TFormValues>;
  name: string;
  label: string;
  disabled: boolean;
  min?: number;
  max?: number;
  step?: string;
}) {
  return (
    <FormField
      control={form.control}
      name={name as never}
      render={({ field }) => (
        <FormItem>
          <FormLabel>{label}</FormLabel>
          <FormControl>
            <Input
              ref={field.ref}
              name={field.name}
              type='number'
              min={min}
              max={max}
              step={step}
              value={field.value}
              onBlur={field.onBlur}
              onChange={(event) => field.onChange(event.target.value)}
              disabled={disabled}
            />
          </FormControl>
        </FormItem>
      )}
    />
  );
}
