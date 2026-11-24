'use client';

import { useState } from 'react';
import { z } from 'zod';
import { useFieldArray, type FieldPath, type UseFormReturn } from 'react-hook-form';
import {
  IconArrowDown,
  IconArrowUp,
  IconDatabase,
  IconFingerprint,
  IconLock,
  IconPlus,
  IconRoute,
  IconTrash,
} from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { Alert, AlertDescription } from '@/components/ui/alert';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import {
  channelContinuationGuardSchema,
  channelIDRewriteFormatSchema,
  channelIDRewriteIfMissingSchema,
  channelIDRewriteOpSchema,
  channelIDRewriteWhenSchema,
  type ChannelIDRewriteFormat,
  type ChannelIDRewriteRule,
  type ChannelKeySelectionStrategy,
  type ChannelSessionAffinity,
} from '../data/schema';

export const DEFAULT_LIKELY_AFFINITY_TTL_MINUTES = 30;
export const DEFAULT_EXACT_AFFINITY_TTL_MINUTES = 1440;
export const DEFAULT_SESSION_AFFINITY_TTL_SECONDS = 3600;
export const MIN_SESSION_AFFINITY_TTL_SECONDS = 60;
export const MAX_SESSION_AFFINITY_TTL_SECONDS = 604800;

export type ChannelKeyRoutingStrategyValue = 'inherit' | ChannelKeySelectionStrategy;

const PROTECTED_HEADERS = new Set([
  'authorization',
  'proxy-authorization',
  'x-api-key',
  'x-goog-api-key',
  'api-key',
  'host',
  'content-length',
  'transfer-encoding',
  'connection',
  'upgrade',
  'te',
  'trailer',
  'keep-alive',
  'cookie',
  'set-cookie',
  'content-encoding',
  'content-type',
]);

const CONTINUATION_OWNED_JSON_PATHS = new Set(['previous_response_id']);
const HEADER_NAME_RE = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;
const INVALID_JSON_PATH_CHARS_RE = /[*?#@[\]|\\,:!\s]/;
const PRINTABLE_ASCII_RE = /^[\x20-\x7e]*$/;

function validateTargetOrAnchor(raw: string, forbidContinuationOwned: boolean): string | null {
  const trimmed = raw.trim();
  if (!trimmed) {
    return 'channels.dialogs.keyRouting.validation.required';
  }
  if (trimmed.startsWith('header:')) {
    const name = trimmed.slice('header:'.length).trim();
    if (!name || !HEADER_NAME_RE.test(name)) {
      return 'channels.dialogs.keyRouting.validation.invalidHeader';
    }
    if (PROTECTED_HEADERS.has(name.toLowerCase())) {
      return 'channels.dialogs.keyRouting.validation.protectedHeader';
    }
    return null;
  }
  if (trimmed.startsWith('json:')) {
    const path = trimmed.slice('json:'.length).trim();
    if (!path || path.length > 256 || path.startsWith('.') || path.endsWith('.') || path.includes('..')) {
      return 'channels.dialogs.keyRouting.validation.invalidJsonPath';
    }
    if (INVALID_JSON_PATH_CHARS_RE.test(path) || !PRINTABLE_ASCII_RE.test(path)) {
      return 'channels.dialogs.keyRouting.validation.literalJsonPathOnly';
    }
    const segments = path.split('.');
    if (segments.length > 8 || segments.some((s) => !s)) {
      return 'channels.dialogs.keyRouting.validation.invalidJsonPath';
    }
    if (forbidContinuationOwned && CONTINUATION_OWNED_JSON_PATHS.has(path.toLowerCase())) {
      return 'channels.dialogs.keyRouting.validation.continuationOwnedPath';
    }
    return null;
  }
  return 'channels.dialogs.keyRouting.validation.targetPrefix';
}

export const sessionAffinityFormSchema = z
  .object({
    enabled: z.boolean(),
    ttlSeconds: z.coerce
      .number()
      .int()
      .min(MIN_SESSION_AFFINITY_TTL_SECONDS)
      .max(MAX_SESSION_AFFINITY_TTL_SECONDS),
    anchors: z.array(z.string()).max(16),
    continuationGuard: channelContinuationGuardSchema,
  })
  .superRefine((val, ctx) => {
    val.anchors.forEach((anchor, idx) => {
      const err = validateTargetOrAnchor(anchor, false);
      if (err) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: err,
          path: ['anchors', idx],
        });
      }
    });
  });

export const idRewriteRuleFormSchema = z
  .object({
    target: z.string(),
    op: channelIDRewriteOpSchema,
    when: channelIDRewriteWhenSchema,
    format: channelIDRewriteFormatSchema,
    prefix: z.string().optional(),
    ifMissing: channelIDRewriteIfMissingSchema,
  })
  .superRefine((rule, ctx) => {
    const targetErr = validateTargetOrAnchor(rule.target, true);
    if (targetErr) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: targetErr,
        path: ['target'],
      });
    }
    if (rule.op === 'derive' && (rule.format === 'prefix_hex8' || rule.format === 'prefix_hex16')) {
      const prefix = rule.prefix ?? '';
      if (prefix.length > 32) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: 'channels.dialogs.keyRouting.validation.prefixTooLong',
          path: ['prefix'],
        });
      } else if (!PRINTABLE_ASCII_RE.test(prefix)) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: 'channels.dialogs.keyRouting.validation.prefixAsciiOnly',
          path: ['prefix'],
        });
      }
    }
  });

export type SessionAffinityFormValues = z.output<typeof sessionAffinityFormSchema>;
export type IDRewriteRuleFormValues = z.output<typeof idRewriteRuleFormSchema>;

export type KeyRoutingFormValues = {
  strategy: ChannelKeyRoutingStrategyValue;
  likelyAffinityTTLMinutes: number;
  exactAffinityTTLMinutes: number;
  sessionAffinity: SessionAffinityFormValues;
  idRewriteRules: IDRewriteRuleFormValues[];
};

export function sessionAffinityValuesFromStored(stored?: ChannelSessionAffinity | null): SessionAffinityFormValues {
  return {
    enabled: stored?.enabled ?? false,
    ttlSeconds: stored?.ttlSeconds ?? DEFAULT_SESSION_AFFINITY_TTL_SECONDS,
    anchors: [...(stored?.anchors ?? [])],
    continuationGuard: stored?.continuationGuard === 'off' ? 'off' : 'strict_replay',
  };
}

export function idRewriteRulesValuesFromStored(stored?: ChannelIDRewriteRule[] | null): IDRewriteRuleFormValues[] {
  return (stored ?? []).map((rule) => ({
    target: rule.target,
    op: rule.op,
    when: rule.when === 'on_key_switch' ? 'on_key_switch' : 'always',
    format: rule.format ?? 'hex32',
    prefix: rule.prefix ?? '',
    ifMissing: rule.ifMissing === 'generate' ? 'generate' : 'skip',
  }));
}

export function sessionAffinityToInput(values: SessionAffinityFormValues): ChannelSessionAffinity {
  return {
    enabled: values.enabled,
    ttlSeconds: values.ttlSeconds,
    anchors: values.anchors.map((a) => a.trim()).filter(Boolean),
    continuationGuard: values.continuationGuard,
  };
}

export function idRewriteRulesToInput(rules: IDRewriteRuleFormValues[]): ChannelIDRewriteRule[] {
  return rules.map((rule) => {
    const trimmedTarget = rule.target.trim();
    if (rule.op === 'delete') {
      return {
        target: trimmedTarget,
        op: 'delete',
        when: rule.when,
        ifMissing: 'skip',
      };
    }
    const needsPrefix = rule.format === 'prefix_hex8' || rule.format === 'prefix_hex16';
    return {
      target: trimmedTarget,
      op: 'derive',
      when: rule.when,
      format: rule.format,
      prefix: needsPrefix ? (rule.prefix ?? '') : undefined,
      ifMissing: rule.ifMissing,
    };
  });
}

export type RoutingPresetKey = 'kimi_coding' | 'claude_code' | 'codex_responses';

export const ROUTING_PRESET_DRAFTS: Record<
  RoutingPresetKey,
  {
    sessionAffinity: SessionAffinityFormValues;
    idRewriteRules: IDRewriteRuleFormValues[];
  }
> = {
  kimi_coding: {
    sessionAffinity: {
      enabled: true,
      ttlSeconds: 3600,
      anchors: ['header:X-Msh-Session-Id', 'header:X-Session-Id', 'json:metadata.user_id'],
      continuationGuard: 'strict_replay',
    },
    idRewriteRules: [
      { target: 'header:X-Msh-Device-Id', op: 'derive', when: 'always', format: 'hex32', prefix: '', ifMissing: 'skip' },
      { target: 'header:X-Msh-Device-Name', op: 'derive', when: 'always', format: 'prefix_hex8', prefix: 'PC-', ifMissing: 'skip' },
      { target: 'header:X-Msh-Session-Id', op: 'derive', when: 'always', format: 'uuid_v8', prefix: '', ifMissing: 'skip' },
      { target: 'json:metadata.user_id', op: 'derive', when: 'always', format: 'anthropic_user_id', prefix: '', ifMissing: 'skip' },
    ],
  },
  claude_code: {
    sessionAffinity: {
      enabled: true,
      ttlSeconds: 3600,
      anchors: ['header:X-Session-Id', 'json:metadata.user_id'],
      continuationGuard: 'strict_replay',
    },
    idRewriteRules: [
      { target: 'json:metadata.user_id', op: 'derive', when: 'always', format: 'anthropic_user_id', prefix: '', ifMissing: 'skip' },
    ],
  },
  codex_responses: {
    sessionAffinity: {
      enabled: true,
      ttlSeconds: 3600,
      anchors: ['header:Session_id', 'json:prompt_cache_key'],
      continuationGuard: 'strict_replay',
    },
    idRewriteRules: [
      { target: 'header:Session_id', op: 'derive', when: 'always', format: 'uuid_v8', prefix: '', ifMissing: 'skip' },
      { target: 'json:prompt_cache_key', op: 'derive', when: 'always', format: 'hex32', prefix: '', ifMissing: 'skip' },
      { target: 'json:safety_identifier', op: 'derive', when: 'always', format: 'hex32', prefix: '', ifMissing: 'skip' },
    ],
  },
};

/**
 * Maps backend GraphQL validation errors (e.g. "idRewriteRules[1].target ..." or
 * "sessionAffinity.anchors[0] ...") directly to the exact react-hook-form field.
 */
export function applyRoutingServerValidationError<TFormValues extends KeyRoutingFormValues>(
  form: UseFormReturn<TFormValues, unknown, TFormValues>,
  error: unknown
): boolean {
  const message = error instanceof Error ? error.message : String(error ?? '');
  if (!message) return false;

  const ruleFieldMatch = message.match(/idRewriteRules\[(\d+)\]\.(target|op|when|format|prefix|ifMissing)/);
  if (ruleFieldMatch) {
    const [, idx, field] = ruleFieldMatch;
    form.setError(`idRewriteRules.${idx}.${field}` as FieldPath<TFormValues>, {
      type: 'server',
      message,
    });
    return true;
  }

  const anchorMatch = message.match(/sessionAffinity\.anchors\[(\d+)\]/);
  if (anchorMatch) {
    const [, idx] = anchorMatch;
    form.setError(`sessionAffinity.anchors.${idx}` as FieldPath<TFormValues>, {
      type: 'server',
      message,
    });
    return true;
  }

  if (message.includes('sessionAffinity.ttlSeconds')) {
    form.setError('sessionAffinity.ttlSeconds' as FieldPath<TFormValues>, {
      type: 'server',
      message,
    });
    return true;
  }

  if (message.includes('sessionAffinity.continuationGuard')) {
    form.setError('sessionAffinity.continuationGuard' as FieldPath<TFormValues>, {
      type: 'server',
      message,
    });
    return true;
  }

  return false;
}

const STRATEGIES: ChannelKeyRoutingStrategyValue[] = [
  'inherit',
  'trace_sticky',
  'cache_affinity',
  'round_robin',
  'random',
  'quota_reset_priority',
  'smart',
];

const ID_REWRITE_FORMATS: ChannelIDRewriteFormat[] = [
  'hex32',
  'hex64',
  'prefix_hex8',
  'prefix_hex16',
  'uuid_v8',
  'uuid_v5',
  'anthropic_user_id',
];

function isPrefixFormat(format?: string | null): boolean {
  return format === 'prefix_hex8' || format === 'prefix_hex16';
}

interface KeyRoutingPanelProps<TFormValues extends KeyRoutingFormValues> {
  form: UseFormReturn<TFormValues, unknown, TFormValues>;
  globalStrategy?: ChannelKeySelectionStrategy | null;
  disabled?: boolean;
}

export function KeyRoutingPanel<TFormValues extends KeyRoutingFormValues>({
  form,
  globalStrategy,
  disabled = false,
}: KeyRoutingPanelProps<TFormValues>) {
  const { t } = useTranslation();
  const [newAnchorInput, setNewAnchorInput] = useState('');

  const selectedStrategy = form.watch('strategy' as never) as ChannelKeyRoutingStrategyValue;
  const effectiveStrategy = selectedStrategy === 'inherit' ? (globalStrategy ?? 'trace_sticky') : selectedStrategy;
  const sessionAffinityEnabled = form.watch('sessionAffinity.enabled' as never) as boolean;
  const anchors = (form.watch('sessionAffinity.anchors' as never) as string[] | undefined) ?? [];
  const watchedRules = (form.watch('idRewriteRules' as never) as IDRewriteRuleFormValues[] | undefined) ?? [];

  const { fields: ruleFields, append: appendRule, remove: removeRule, swap: swapRules, replace: replaceRules } =
    useFieldArray({
      control: form.control,
      name: 'idRewriteRules' as never,
    });

  const translateValidationMessage = (msg?: string) => {
    if (!msg) return null;
    if (msg.startsWith('channels.')) {
      return t(msg);
    }
    return msg;
  };

  const handleAddAnchor = () => {
    const trimmed = newAnchorInput.trim();
    if (!trimmed || anchors.length >= 16) return;
    form.setValue('sessionAffinity.anchors' as never, [...anchors, trimmed] as never, {
      shouldDirty: true,
      shouldValidate: true,
    });
    setNewAnchorInput('');
  };

  const handleMoveAnchor = (idx: number, delta: -1 | 1) => {
    const targetIdx = idx + delta;
    if (targetIdx < 0 || targetIdx >= anchors.length) return;
    const next = [...anchors];
    const [item] = next.splice(idx, 1);
    next.splice(targetIdx, 0, item);
    form.setValue('sessionAffinity.anchors' as never, next as never, {
      shouldDirty: true,
      shouldValidate: true,
    });
  };

  const handleRemoveAnchor = (idx: number) => {
    const next = anchors.filter((_, i) => i !== idx);
    form.setValue('sessionAffinity.anchors' as never, next as never, {
      shouldDirty: true,
      shouldValidate: true,
    });
  };

  const applyPresetDraft = (presetKey: RoutingPresetKey) => {
    const draft = ROUTING_PRESET_DRAFTS[presetKey];
    form.setValue('sessionAffinity.enabled' as never, draft.sessionAffinity.enabled as never, {
      shouldDirty: true,
      shouldValidate: true,
    });
    form.setValue('sessionAffinity.ttlSeconds' as never, draft.sessionAffinity.ttlSeconds as never, {
      shouldDirty: true,
      shouldValidate: true,
    });
    form.setValue('sessionAffinity.anchors' as never, [...draft.sessionAffinity.anchors] as never, {
      shouldDirty: true,
      shouldValidate: true,
    });
    form.setValue('sessionAffinity.continuationGuard' as never, draft.sessionAffinity.continuationGuard as never, {
      shouldDirty: true,
      shouldValidate: true,
    });
    replaceRules(draft.idRewriteRules.map((rule) => ({ ...rule })) as never);
  };

  const handleClearRules = () => {
    replaceRules([] as never);
    form.setValue('idRewriteRules' as never, [] as never, {
      shouldDirty: true,
      shouldValidate: true,
    });
  };

  const handleAddRule = () => {
    if (ruleFields.length >= 32) return;
    appendRule({
      target: '',
      op: 'derive',
      when: 'always',
      format: 'hex32',
      prefix: '',
      ifMissing: 'skip',
    } as never);
  };

  const anchorErrors = (form.formState.errors as Record<string, any>)?.sessionAffinity?.anchors as
    | Array<{ message?: string } | undefined>
    | undefined;
  const ruleErrors = (form.formState.errors as Record<string, any>)?.idRewriteRules as
    | Array<Record<string, { message?: string }> | undefined>
    | undefined;

  return (
    <div className='space-y-4'>
      {/* 1. Candidate Ranking Strategy */}
      <Card>
        <CardHeader className='pb-3'>
          <CardTitle className='flex items-center gap-2 text-base'>
            <IconRoute className='h-4 w-4' />
            {t('channels.dialogs.keys.routing.title')}
          </CardTitle>
          <CardDescription>{t('channels.dialogs.keys.routing.description')}</CardDescription>
        </CardHeader>
        <CardContent className='space-y-4'>
          <FormField
            control={form.control}
            name={'strategy' as never}
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('channels.dialogs.keyRouting.fields.strategy.label')}</FormLabel>
                <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                  <FormControl>
                    <SelectTrigger>
                      <SelectValue placeholder={t('channels.dialogs.keyRouting.fields.strategy.placeholder')} />
                    </SelectTrigger>
                  </FormControl>
                  <SelectContent>
                    {STRATEGIES.map((strategy) => (
                      <SelectItem key={strategy} value={strategy}>
                        {strategy === 'inherit'
                          ? t('channels.dialogs.keyRouting.strategies.inherit.label', {
                              strategy: t(`channels.dialogs.keyRouting.strategies.${globalStrategy ?? 'trace_sticky'}.label`),
                            })
                          : t(`channels.dialogs.keyRouting.strategies.${strategy}.label`)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <FormDescription>
                  {selectedStrategy === 'inherit'
                    ? t('channels.dialogs.keyRouting.fields.strategy.inheritDescription', {
                        strategy: t(`channels.dialogs.keyRouting.strategies.${globalStrategy ?? 'trace_sticky'}.label`),
                      })
                    : t('channels.dialogs.keyRouting.fields.strategy.description')}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          {effectiveStrategy === 'cache_affinity' && selectedStrategy !== 'inherit' ? (
            sessionAffinityEnabled ? (
              <div className='bg-muted/40 text-muted-foreground rounded-md border px-3 py-2 text-xs'>
                {t('channels.dialogs.keyRouting.sessionAffinity.legacyTtlSuperseded')}
              </div>
            ) : (
              <div className='grid gap-4 md:grid-cols-2'>
                <FormField
                  control={form.control}
                  name={'likelyAffinityTTLMinutes' as never}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.keyRouting.fields.likelyAffinityTTLMinutes.label')}</FormLabel>
                      <FormControl>
                        <Input
                          type='number'
                          min={1}
                          max={1440}
                          placeholder={String(DEFAULT_LIKELY_AFFINITY_TTL_MINUTES)}
                          disabled={disabled}
                          {...field}
                        />
                      </FormControl>
                      <FormDescription>{t('channels.dialogs.keyRouting.fields.likelyAffinityTTLMinutes.description')}</FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name={'exactAffinityTTLMinutes' as never}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.keyRouting.fields.exactAffinityTTLMinutes.label')}</FormLabel>
                      <FormControl>
                        <Input
                          type='number'
                          min={1}
                          max={10080}
                          placeholder={String(DEFAULT_EXACT_AFFINITY_TTL_MINUTES)}
                          disabled={disabled}
                          {...field}
                        />
                      </FormControl>
                      <FormDescription>{t('channels.dialogs.keyRouting.fields.exactAffinityTTLMinutes.description')}</FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              </div>
            )
          ) : null}

          <Alert>
            <IconDatabase className='h-4 w-4' />
            <AlertDescription>
              <div className='font-medium'>
                {selectedStrategy === 'inherit'
                  ? t('channels.dialogs.keyRouting.strategies.inherit.effective', {
                      strategy: t(`channels.dialogs.keyRouting.strategies.${effectiveStrategy}.label`),
                    })
                  : t(`channels.dialogs.keyRouting.strategies.${effectiveStrategy}.label`)}
              </div>
              <div className='mt-1 text-sm'>
                {selectedStrategy === 'inherit'
                  ? t('channels.dialogs.keyRouting.strategies.inherit.description')
                  : t(`channels.dialogs.keyRouting.strategies.${effectiveStrategy}.description`)}
              </div>
            </AlertDescription>
          </Alert>
        </CardContent>
      </Card>

      {/* 2. Session Affinity */}
      <Card>
        <CardHeader className='pb-3'>
          <div className='flex flex-wrap items-center justify-between gap-3'>
            <div>
              <CardTitle className='flex items-center gap-2 text-base'>
                <IconLock className='h-4 w-4' />
                {t('channels.dialogs.keyRouting.sessionAffinity.title')}
              </CardTitle>
              <CardDescription>{t('channels.dialogs.keyRouting.sessionAffinity.description')}</CardDescription>
            </div>
            <FormField
              control={form.control}
              name={'sessionAffinity.enabled' as never}
              render={({ field }) => (
                <FormItem className='flex items-center gap-2 space-y-0'>
                  <FormLabel className='text-xs font-medium'>
                    {t('channels.dialogs.keyRouting.sessionAffinity.enabledLabel')}
                  </FormLabel>
                  <FormControl>
                    <Switch checked={Boolean(field.value)} onCheckedChange={field.onChange} disabled={disabled} />
                  </FormControl>
                </FormItem>
              )}
            />
          </div>
        </CardHeader>

        {sessionAffinityEnabled ? (
          <CardContent className='space-y-4'>
            <div className='grid gap-4 md:grid-cols-2'>
              <FormField
                control={form.control}
                name={'sessionAffinity.ttlSeconds' as never}
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('channels.dialogs.keyRouting.sessionAffinity.ttlSeconds.label')}</FormLabel>
                    <FormControl>
                      <Input
                        type='number'
                        min={MIN_SESSION_AFFINITY_TTL_SECONDS}
                        max={MAX_SESSION_AFFINITY_TTL_SECONDS}
                        value={field.value ?? DEFAULT_SESSION_AFFINITY_TTL_SECONDS}
                        onChange={(e) => field.onChange(e.target.value)}
                        onBlur={field.onBlur}
                        disabled={disabled}
                      />
                    </FormControl>
                    <FormDescription>{t('channels.dialogs.keyRouting.sessionAffinity.ttlSeconds.description')}</FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />

              <FormField
                control={form.control}
                name={'sessionAffinity.continuationGuard' as never}
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('channels.dialogs.keyRouting.sessionAffinity.continuationGuard.label')}</FormLabel>
                    <Select value={field.value ?? 'strict_replay'} onValueChange={field.onChange} disabled={disabled}>
                      <FormControl>
                        <SelectTrigger>
                          <SelectValue />
                        </SelectTrigger>
                      </FormControl>
                      <SelectContent>
                        <SelectItem value='strict_replay'>
                          {t('channels.dialogs.keyRouting.sessionAffinity.continuationGuard.strict_replay')}
                        </SelectItem>
                        <SelectItem value='off'>
                          {t('channels.dialogs.keyRouting.sessionAffinity.continuationGuard.off')}
                        </SelectItem>
                      </SelectContent>
                    </Select>
                    <FormDescription>
                      {t('channels.dialogs.keyRouting.sessionAffinity.continuationGuard.description')}
                    </FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </div>

            <div className='space-y-2'>
              <div className='flex items-center justify-between'>
                <FormLabel>{t('channels.dialogs.keyRouting.sessionAffinity.anchors.label')}</FormLabel>
                <span className='text-muted-foreground font-mono text-[11px]'>{anchors.length}/16</span>
              </div>

              {anchors.length > 0 ? (
                <div className='space-y-1.5'>
                  {anchors.map((anchor, idx) => {
                    const errMsg = translateValidationMessage(anchorErrors?.[idx]?.message);
                    return (
                      <div key={`anchor-${idx}`} className='space-y-1'>
                        <div className='flex items-center gap-1.5'>
                          <Badge variant='outline' className='w-6 justify-center px-0 font-mono text-[10px]'>
                            {idx + 1}
                          </Badge>
                          <Input
                            value={anchor}
                            onChange={(e) => {
                              const next = [...anchors];
                              next[idx] = e.target.value;
                              form.setValue('sessionAffinity.anchors' as never, next as never, {
                                shouldDirty: true,
                                shouldValidate: true,
                              });
                            }}
                            placeholder='header:X-Session-Id or json:metadata.user_id'
                            className='h-8 font-mono text-xs'
                            disabled={disabled}
                          />
                          <Button
                            type='button'
                            size='icon'
                            variant='ghost'
                            className='h-8 w-8 shrink-0'
                            disabled={disabled || idx === 0}
                            onClick={() => handleMoveAnchor(idx, -1)}
                            aria-label={t('channels.dialogs.keyRouting.actions.moveUp')}
                          >
                            <IconArrowUp className='h-3.5 w-3.5' />
                          </Button>
                          <Button
                            type='button'
                            size='icon'
                            variant='ghost'
                            className='h-8 w-8 shrink-0'
                            disabled={disabled || idx === anchors.length - 1}
                            onClick={() => handleMoveAnchor(idx, 1)}
                            aria-label={t('channels.dialogs.keyRouting.actions.moveDown')}
                          >
                            <IconArrowDown className='h-3.5 w-3.5' />
                          </Button>
                          <Button
                            type='button'
                            size='icon'
                            variant='ghost'
                            className='text-destructive h-8 w-8 shrink-0'
                            disabled={disabled}
                            onClick={() => handleRemoveAnchor(idx)}
                            aria-label={t('common.buttons.remove')}
                          >
                            <IconTrash className='h-3.5 w-3.5' />
                          </Button>
                        </div>
                        {errMsg ? <p className='text-destructive pl-8 text-xs'>{errMsg}</p> : null}
                      </div>
                    );
                  })}
                </div>
              ) : (
                <div className='text-muted-foreground rounded-md border border-dashed px-3 py-2 text-xs'>
                  {t('channels.dialogs.keyRouting.sessionAffinity.anchors.empty')}
                </div>
              )}

              <div className='flex items-center gap-2 pt-1'>
                <Input
                  value={newAnchorInput}
                  onChange={(e) => setNewAnchorInput(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') {
                      e.preventDefault();
                      handleAddAnchor();
                    }
                  }}
                  placeholder={t('channels.dialogs.keyRouting.sessionAffinity.anchors.placeholder')}
                  className='h-8 font-mono text-xs'
                  disabled={disabled || anchors.length >= 16}
                />
                <Button
                  type='button'
                  size='sm'
                  variant='outline'
                  className='h-8 shrink-0 text-xs'
                  disabled={disabled || !newAnchorInput.trim() || anchors.length >= 16}
                  onClick={handleAddAnchor}
                >
                  <IconPlus className='mr-1 h-3.5 w-3.5' />
                  {t('channels.dialogs.keyRouting.sessionAffinity.anchors.add')}
                </Button>
              </div>
            </div>
          </CardContent>
        ) : null}
      </Card>

      {/* 3. ID Rewrite Rules */}
      <Card>
        <CardHeader className='pb-3'>
          <div className='flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between'>
            <div>
              <CardTitle className='flex items-center gap-2 text-base'>
                <IconFingerprint className='h-4 w-4' />
                {t('channels.dialogs.keyRouting.idRewrite.title')}
              </CardTitle>
              <CardDescription>{t('channels.dialogs.keyRouting.idRewrite.description')}</CardDescription>
            </div>
            <div className='flex flex-wrap items-center gap-1.5'>
              <Button
                type='button'
                size='sm'
                variant='outline'
                className='h-7 px-2.5 text-xs'
                disabled={disabled}
                onClick={() => applyPresetDraft('kimi_coding')}
                data-testid='routing-preset-kimi'
              >
                Kimi Coding
              </Button>
              <Button
                type='button'
                size='sm'
                variant='outline'
                className='h-7 px-2.5 text-xs'
                disabled={disabled}
                onClick={() => applyPresetDraft('claude_code')}
                data-testid='routing-preset-claude'
              >
                Claude Code
              </Button>
              <Button
                type='button'
                size='sm'
                variant='outline'
                className='h-7 px-2.5 text-xs'
                disabled={disabled}
                onClick={() => applyPresetDraft('codex_responses')}
                data-testid='routing-preset-codex'
              >
                Codex Responses
              </Button>
              {ruleFields.length > 0 ? (
                <Button
                  type='button'
                  size='sm'
                  variant='ghost'
                  className='text-muted-foreground hover:text-destructive h-7 px-2 text-xs'
                  disabled={disabled}
                  onClick={handleClearRules}
                  data-testid='routing-clear-rules'
                >
                  {t('channels.dialogs.keyRouting.idRewrite.clearRules')}
                </Button>
              ) : null}
            </div>
          </div>
        </CardHeader>

        <CardContent className='space-y-3'>
          {ruleFields.length === 0 ? (
            <div className='text-muted-foreground rounded-md border border-dashed px-3 py-4 text-center text-xs'>
              {t('channels.dialogs.keyRouting.idRewrite.empty')}
            </div>
          ) : (
            <div className='space-y-2.5'>
              {ruleFields.map((fieldItem, idx) => {
                const currentRule = watchedRules[idx];
                const isDerive = (currentRule?.op ?? 'derive') === 'derive';
                const showPrefix = isDerive && isPrefixFormat(currentRule?.format);
                const rowError = ruleErrors?.[idx];

                return (
                  <div
                    key={fieldItem.id}
                    data-testid={`id-rewrite-rule-row-${idx}`}
                    className='bg-muted/20 space-y-2.5 rounded-lg border p-3'
                  >
                    <div className='flex items-center justify-between gap-2'>
                      <Badge variant='outline' className='font-mono text-[10px]'>
                        #{idx + 1}
                      </Badge>
                      <div className='flex items-center gap-1'>
                        <Button
                          type='button'
                          size='icon'
                          variant='ghost'
                          className='h-7 w-7'
                          disabled={disabled || idx === 0}
                          onClick={() => swapRules(idx, idx - 1)}
                          aria-label={t('channels.dialogs.keyRouting.actions.moveUp')}
                        >
                          <IconArrowUp className='h-3.5 w-3.5' />
                        </Button>
                        <Button
                          type='button'
                          size='icon'
                          variant='ghost'
                          className='h-7 w-7'
                          disabled={disabled || idx === ruleFields.length - 1}
                          onClick={() => swapRules(idx, idx + 1)}
                          aria-label={t('channels.dialogs.keyRouting.actions.moveDown')}
                        >
                          <IconArrowDown className='h-3.5 w-3.5' />
                        </Button>
                        <Button
                          type='button'
                          size='icon'
                          variant='ghost'
                          className='text-destructive h-7 w-7'
                          disabled={disabled}
                          onClick={() => removeRule(idx)}
                          aria-label={t('common.buttons.remove')}
                        >
                          <IconTrash className='h-3.5 w-3.5' />
                        </Button>
                      </div>
                    </div>

                    <div className='grid gap-2.5 md:grid-cols-12'>
                      <div className='space-y-1 md:col-span-6'>
                        <FormField
                          control={form.control}
                          name={`idRewriteRules.${idx}.target` as never}
                          render={({ field }) => (
                            <FormItem className='space-y-1'>
                              <FormLabel className='text-xs'>
                                {t('channels.dialogs.keyRouting.idRewrite.fields.target')}
                              </FormLabel>
                              <FormControl>
                                <Input
                                  {...field}
                                  placeholder='header:X-Msh-Device-Id or json:metadata.user_id'
                                  className='h-8 font-mono text-xs'
                                  disabled={disabled}
                                />
                              </FormControl>
                              {rowError?.target?.message ? (
                                <p className='text-destructive text-xs'>
                                  {translateValidationMessage(rowError.target.message)}
                                </p>
                              ) : null}
                            </FormItem>
                          )}
                        />
                      </div>

                      <div className='space-y-1 md:col-span-3'>
                        <FormField
                          control={form.control}
                          name={`idRewriteRules.${idx}.op` as never}
                          render={({ field }) => (
                            <FormItem className='space-y-1'>
                              <FormLabel className='text-xs'>
                                {t('channels.dialogs.keyRouting.idRewrite.fields.op')}
                              </FormLabel>
                              <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                                <FormControl>
                                  <SelectTrigger className='h-8 text-xs'>
                                    <SelectValue />
                                  </SelectTrigger>
                                </FormControl>
                                <SelectContent>
                                  <SelectItem value='derive'>
                                    {t('channels.dialogs.keyRouting.idRewrite.ops.derive')}
                                  </SelectItem>
                                  <SelectItem value='delete'>
                                    {t('channels.dialogs.keyRouting.idRewrite.ops.delete')}
                                  </SelectItem>
                                </SelectContent>
                              </Select>
                            </FormItem>
                          )}
                        />
                      </div>

                      <div className='space-y-1 md:col-span-3'>
                        <FormField
                          control={form.control}
                          name={`idRewriteRules.${idx}.when` as never}
                          render={({ field }) => (
                            <FormItem className='space-y-1'>
                              <FormLabel className='text-xs'>
                                {t('channels.dialogs.keyRouting.idRewrite.fields.when')}
                              </FormLabel>
                              <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                                <FormControl>
                                  <SelectTrigger className='h-8 text-xs'>
                                    <SelectValue />
                                  </SelectTrigger>
                                </FormControl>
                                <SelectContent>
                                  <SelectItem value='always'>
                                    {t('channels.dialogs.keyRouting.idRewrite.when.always')}
                                  </SelectItem>
                                  <SelectItem value='on_key_switch'>
                                    {t('channels.dialogs.keyRouting.idRewrite.when.on_key_switch')}
                                  </SelectItem>
                                </SelectContent>
                              </Select>
                            </FormItem>
                          )}
                        />
                      </div>

                      {isDerive ? (
                        <>
                          <div className={showPrefix ? 'space-y-1 md:col-span-4' : 'space-y-1 md:col-span-6'}>
                            <FormField
                              control={form.control}
                              name={`idRewriteRules.${idx}.format` as never}
                              render={({ field }) => (
                                <FormItem className='space-y-1'>
                                  <FormLabel className='text-xs'>
                                    {t('channels.dialogs.keyRouting.idRewrite.fields.format')}
                                  </FormLabel>
                                  <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                                    <FormControl>
                                      <SelectTrigger className='h-8 text-xs'>
                                        <SelectValue />
                                      </SelectTrigger>
                                    </FormControl>
                                    <SelectContent>
                                      {ID_REWRITE_FORMATS.map((fmt) => (
                                        <SelectItem key={fmt} value={fmt}>
                                          {fmt}
                                        </SelectItem>
                                      ))}
                                    </SelectContent>
                                  </Select>
                                </FormItem>
                              )}
                            />
                          </div>

                          {showPrefix ? (
                            <div className='space-y-1 md:col-span-4'>
                              <FormField
                                control={form.control}
                                name={`idRewriteRules.${idx}.prefix` as never}
                                render={({ field }) => (
                                  <FormItem className='space-y-1'>
                                    <FormLabel className='text-xs'>
                                      {t('channels.dialogs.keyRouting.idRewrite.fields.prefix')}
                                    </FormLabel>
                                    <FormControl>
                                      <Input
                                        {...field}
                                        value={field.value ?? ''}
                                        placeholder='PC-'
                                        maxLength={32}
                                        className='h-8 font-mono text-xs'
                                        disabled={disabled}
                                      />
                                    </FormControl>
                                    {rowError?.prefix?.message ? (
                                      <p className='text-destructive text-xs'>
                                        {translateValidationMessage(rowError.prefix.message)}
                                      </p>
                                    ) : null}
                                  </FormItem>
                                )}
                              />
                            </div>
                          ) : null}

                          <div className={showPrefix ? 'space-y-1 md:col-span-4' : 'space-y-1 md:col-span-6'}>
                            <FormField
                              control={form.control}
                              name={`idRewriteRules.${idx}.ifMissing` as never}
                              render={({ field }) => (
                                <FormItem className='space-y-1'>
                                  <FormLabel className='text-xs'>
                                    {t('channels.dialogs.keyRouting.idRewrite.fields.ifMissing')}
                                  </FormLabel>
                                  <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                                    <FormControl>
                                      <SelectTrigger className='h-8 text-xs'>
                                        <SelectValue />
                                      </SelectTrigger>
                                    </FormControl>
                                    <SelectContent>
                                      <SelectItem value='skip'>
                                        {t('channels.dialogs.keyRouting.idRewrite.ifMissing.skip')}
                                      </SelectItem>
                                      <SelectItem value='generate'>
                                        {t('channels.dialogs.keyRouting.idRewrite.ifMissing.generate')}
                                      </SelectItem>
                                    </SelectContent>
                                  </Select>
                                </FormItem>
                              )}
                            />
                          </div>
                        </>
                      ) : null}
                    </div>
                  </div>
                );
              })}
            </div>
          )}

          <div className='flex items-center justify-between pt-1'>
            <Button
              type='button'
              size='sm'
              variant='outline'
              className='h-8 text-xs'
              disabled={disabled || ruleFields.length >= 32}
              onClick={handleAddRule}
              data-testid='routing-add-rule'
            >
              <IconPlus className='mr-1.5 h-3.5 w-3.5' />
              {t('channels.dialogs.keyRouting.idRewrite.addRule')}
            </Button>
            <span className='text-muted-foreground font-mono text-[11px]'>{ruleFields.length}/32</span>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
