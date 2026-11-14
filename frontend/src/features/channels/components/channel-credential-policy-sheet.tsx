'use client';

import { useEffect, useMemo, useState } from 'react';
import { ChevronDown, Loader2, Shield, SlidersHorizontal } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { cn, extractNumberIDAsNumber } from '@/lib/utils';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet';
import { Switch } from '@/components/ui/switch';
import { useOutboundProxies, useOutboundProxyPools } from '@/features/system/data/outbound-proxies';
import { useUpsertChannelCredentialPolicy } from '../data/channels';
import type {
  ChannelAPIKeyInventoryItem,
  ChannelCredentialPolicy,
  CredentialEgressFailPolicy,
  CredentialEgressMode,
} from '../data/schema';

const EGRESS_MODES: CredentialEgressMode[] = [
  'inherit_channel',
  'direct',
  'pinned_proxy',
  'proxy_pool',
  'custom_url',
];

const FAIL_POLICIES: CredentialEgressFailPolicy[] = [
  'fail_closed',
  'backup_proxy',
  'pool_fallback',
];

export interface CredentialPolicyTarget {
  id: string;
  maskedKey: string;
  policy?: ChannelCredentialPolicy | null;
  effectiveProxyName?: string | null;
  effectiveExitIP?: string | null;
  inFlight?: number;
  quotaRemainingRatio?: number | null;
  quotaResetAt?: string | null;
}

export function CredentialPolicySummary({ item }: { item: CredentialPolicyTarget }) {
  const { t } = useTranslation();
  const policy = item.policy;
  const egressMode = policy?.egressMode ?? 'inherit_channel';
  const priority = policy?.priority ?? 0;
  const weight = policy?.weight ?? 100;
  const maxConcurrent = policy?.maxConcurrent;
  const inFlight = item.inFlight ?? 0;

  const egressLabel = useMemo(() => {
    if (item.effectiveProxyName) {
      return item.effectiveExitIP
        ? `${item.effectiveProxyName} (${item.effectiveExitIP})`
        : item.effectiveProxyName;
    }
    if (egressMode === 'direct') {
      return t('channels.dialogs.keys.policy.egressModes.direct');
    }
    if (egressMode === 'proxy_pool' && policy?.proxyPoolName) {
      return `${t('channels.dialogs.keys.policy.badges.pool')}: ${policy.proxyPoolName}`;
    }
    if (egressMode === 'pinned_proxy' && policy?.outboundProxyId) {
      return `${t('channels.dialogs.keys.policy.badges.proxy')} #${policy.outboundProxyId}`;
    }
    if (egressMode === 'custom_url') {
      return t('channels.dialogs.keys.policy.egressModes.custom_url');
    }
    return null;
  }, [egressMode, item.effectiveExitIP, item.effectiveProxyName, policy?.outboundProxyId, policy?.proxyPoolName, t]);

  const hasBadges =
    egressLabel != null ||
    priority !== 0 ||
    (weight > 0 && weight !== 100) ||
    maxConcurrent != null ||
    inFlight > 0;

  if (!hasBadges) {
    return null;
  }

  return (
    <div className='flex flex-wrap items-center gap-1.5 pt-0.5'>
      {egressLabel ? (
        <Badge variant='secondary' className='font-normal text-[11px]'>
          {egressLabel}
        </Badge>
      ) : null}
      {priority !== 0 ? (
        <Badge variant='outline' className='font-mono text-[11px]'>
          P{priority}
        </Badge>
      ) : null}
      {weight > 0 && weight !== 100 ? (
        <Badge variant='outline' className='font-mono text-[11px]'>
          W{weight}
        </Badge>
      ) : null}
      {maxConcurrent != null || inFlight > 0 ? (
        <Badge variant='outline' className='font-mono text-[11px]'>
          {maxConcurrent != null ? `${inFlight}/${maxConcurrent}` : `${inFlight} ${t('channels.dialogs.keys.policy.badges.inFlight')}`}
        </Badge>
      ) : null}
    </div>
  );
}

export function CredentialQuotaIndicator({
  quotaRemainingRatio,
  quotaResetAt,
  language,
}: {
  quotaRemainingRatio?: number | null;
  quotaResetAt?: string | null;
  language: string;
}) {
  const { t } = useTranslation();

  if (quotaRemainingRatio == null && !quotaResetAt) {
    return null;
  }

  const clamped =
    quotaRemainingRatio != null ? Math.max(0, Math.min(1, quotaRemainingRatio)) : null;
  const percent = clamped != null ? Math.round(clamped * 100) : null;

  const formattedReset = useMemo(() => {
    if (!quotaResetAt) return null;
    const date = new Date(quotaResetAt);
    if (Number.isNaN(date.getTime())) return quotaResetAt;
    return new Intl.DateTimeFormat(language.toLowerCase().startsWith('zh') ? 'zh-CN' : 'en-US', {
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      hour12: false,
    }).format(date);
  }, [quotaResetAt, language]);

  return (
    <div className='mt-1.5 space-y-1'>
      {percent != null ? (
        <div className='flex items-center gap-2'>
          <div className='bg-muted h-1.5 w-20 overflow-hidden rounded-full'>
            <div
              className={cn(
                'h-full rounded-full transition-all',
                percent <= 5
                  ? 'bg-destructive'
                  : percent <= 20
                    ? 'bg-amber-500'
                    : 'bg-emerald-500'
              )}
              style={{ width: `${percent}%` }}
            />
          </div>
          <span className='text-muted-foreground font-mono text-xs tabular-nums'>{percent}%</span>
        </div>
      ) : null}
      {formattedReset ? (
        <div className='text-muted-foreground text-[11px]'>
          {t('channels.dialogs.keys.policy.quotaResetsAt', { time: formattedReset })}
        </div>
      ) : null}
    </div>
  );
}

interface ChannelCredentialPolicySheetProps {
  channelID: string;
  target: CredentialPolicyTarget | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSaved?: (updated: ChannelAPIKeyInventoryItem) => void;
}

export function ChannelCredentialPolicySheet({
  channelID,
  target,
  open,
  onOpenChange,
  onSaved,
}: ChannelCredentialPolicySheetProps) {
  const { t } = useTranslation();
  const upsertPolicy = useUpsertChannelCredentialPolicy();
  const { data: outboundProxies = [] } = useOutboundProxies({ enabled: open });
  const { data: outboundProxyPools = [] } = useOutboundProxyPools({ enabled: open });

  const [name, setName] = useState('');
  const [priority, setPriority] = useState('0');
  const [weight, setWeight] = useState('100');
  const [maxConcurrent, setMaxConcurrent] = useState('');
  const [requestIntervalMs, setRequestIntervalMs] = useState('');
  const [manualQuotaLimit, setManualQuotaLimit] = useState('');
  const [egressMode, setEgressMode] = useState<CredentialEgressMode>('inherit_channel');
  const [outboundProxyId, setOutboundProxyId] = useState<string>('');
  const [proxyPoolName, setProxyPoolName] = useState<string>('');
  const [customProxyUrl, setCustomProxyUrl] = useState('');
  const [customProxyUsername, setCustomProxyUsername] = useState('');
  const [customProxyPassword, setCustomProxyPassword] = useState('');
  const [customDisableReuse, setCustomDisableReuse] = useState(false);
  const [egressFailPolicy, setEgressFailPolicy] = useState<CredentialEgressFailPolicy>('fail_closed');
  const [limitsOpen, setLimitsOpen] = useState(false);
  const [validationError, setValidationError] = useState<string | null>(null);

  useEffect(() => {
    if (!open || !target) return;
    setValidationError(null);
    const p = target.policy;
    setName(p?.name || '');
    setPriority(String(p?.priority ?? 0));
    setWeight(String(p?.weight && p.weight > 0 ? p.weight : 100));
    setMaxConcurrent(p?.maxConcurrent != null ? String(p.maxConcurrent) : '');
    setRequestIntervalMs(p?.requestIntervalMs != null ? String(p.requestIntervalMs) : '');
    setManualQuotaLimit(p?.manualQuotaLimit != null ? String(p.manualQuotaLimit) : '');
    setEgressMode(p?.egressMode ?? 'inherit_channel');
    setOutboundProxyId(p?.outboundProxyId != null ? String(p.outboundProxyId) : '');
    setProxyPoolName(p?.proxyPoolName || '');
    setCustomProxyUrl(p?.customProxy?.url || '');
    setCustomProxyUsername(p?.customProxy?.username || '');
    setCustomProxyPassword(p?.customProxy?.password || '');
    setCustomDisableReuse(Boolean(p?.customProxy?.disableConnectionReuse));
    setEgressFailPolicy(p?.egressFailPolicy ?? 'fail_closed');
    setLimitsOpen(p?.requestIntervalMs != null || p?.manualQuotaLimit != null);
  }, [open, target]);

  const proxyByNumericId = useMemo(() => {
    const map = new Map<number, (typeof outboundProxies)[number]>();
    for (const p of outboundProxies) {
      map.set(extractNumberIDAsNumber(p.id), p);
    }
    return map;
  }, [outboundProxies]);

  const selectedManagedProxy = useMemo(() => {
    if (!outboundProxyId) return null;
    return proxyByNumericId.get(Number(outboundProxyId)) ?? null;
  }, [outboundProxyId, proxyByNumericId]);

  const selectedBackupProxy = useMemo(() => {
    if (!selectedManagedProxy?.backupProxyId) return null;
    return proxyByNumericId.get(selectedManagedProxy.backupProxyId) ?? null;
  }, [selectedManagedProxy, proxyByNumericId]);

  const poolOptions = useMemo(() => {
    const names = new Set(outboundProxyPools.map((pool) => pool.name));
    const list = [...outboundProxyPools];
    if (proxyPoolName && !names.has(proxyPoolName)) {
      list.push({
        name: proxyPoolName,
        proxyCount: 0,
        enabledProxyCount: 0,
        credentialReferenceCount: 0,
      });
    }
    return list;
  }, [outboundProxyPools, proxyPoolName]);

  const handleSave = async (event: React.FormEvent) => {
    event.preventDefault();
    if (!target) return;
    setValidationError(null);

    const parsedPriority = Number(priority.trim() || '0');
    if (!Number.isInteger(parsedPriority)) {
      setValidationError(t('channels.dialogs.keys.policy.validation.invalidPriority'));
      return;
    }

    const parsedWeight = Number(weight.trim() || '100');
    if (!Number.isInteger(parsedWeight) || parsedWeight < 1 || parsedWeight > 1000000) {
      setValidationError(t('channels.dialogs.keys.policy.validation.invalidWeight'));
      return;
    }

    let resolvedMaxConcurrent: number | null = null;
    if (maxConcurrent.trim()) {
      const n = Number(maxConcurrent.trim());
      if (!Number.isInteger(n) || n < 1) {
        setValidationError(t('channels.dialogs.keys.policy.validation.invalidMaxConcurrent'));
        return;
      }
      resolvedMaxConcurrent = n;
    }

    let resolvedInterval: number | null = null;
    if (requestIntervalMs.trim()) {
      const n = Number(requestIntervalMs.trim());
      if (!Number.isInteger(n) || n < 0 || n > 86400000) {
        setValidationError(t('channels.dialogs.keys.policy.validation.invalidInterval'));
        return;
      }
      resolvedInterval = n;
    }

    let resolvedManualQuota: number | null = null;
    if (manualQuotaLimit.trim()) {
      const n = Number(manualQuotaLimit.trim());
      if (!Number.isFinite(n) || n <= 0) {
        setValidationError(t('channels.dialogs.keys.policy.validation.invalidManualQuota'));
        return;
      }
      resolvedManualQuota = n;
    }

    if (egressMode === 'pinned_proxy' && (!outboundProxyId || Number(outboundProxyId) <= 0)) {
      setValidationError(t('channels.dialogs.keys.policy.validation.proxyRequired'));
      return;
    }

    if (egressMode === 'proxy_pool' && !proxyPoolName.trim()) {
      setValidationError(t('channels.dialogs.keys.policy.validation.poolRequired'));
      return;
    }

    if (egressMode === 'custom_url' && !customProxyUrl.trim()) {
      setValidationError(t('channels.dialogs.keys.policy.validation.customUrlRequired'));
      return;
    }

    try {
      const updated = await upsertPolicy.mutateAsync({
        channelID,
        input: {
          id: target.id,
          name: name.trim(),
          priority: parsedPriority,
          weight: parsedWeight,
          maxConcurrent: resolvedMaxConcurrent,
          requestIntervalMs: resolvedInterval,
          manualQuotaLimit: resolvedManualQuota,
          egressMode,
          outboundProxyId: egressMode === 'pinned_proxy' ? Number(outboundProxyId) : null,
          proxyPoolName: egressMode === 'proxy_pool' ? proxyPoolName.trim() : '',
          customProxy:
            egressMode === 'custom_url'
              ? {
                  type: 'url',
                  url: customProxyUrl.trim(),
                  username: customProxyUsername.trim() || undefined,
                  password: customProxyPassword || undefined,
                  disableConnectionReuse: customDisableReuse,
                }
              : null,
          egressFailPolicy,
        },
      });
      onSaved?.(updated);
      onOpenChange(false);
    } catch {
      // Handled by hook.
    }
  };

  const showFailover =
    egressMode === 'pinned_proxy' || egressMode === 'proxy_pool' || egressMode === 'custom_url';

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side='right' className='flex w-full flex-col sm:max-w-lg'>
        <SheetHeader className='border-b pb-3 text-left'>
          <SheetTitle className='flex items-center gap-2'>
            <SlidersHorizontal className='h-4 w-4' />
            {t('channels.dialogs.keys.policy.title')}
          </SheetTitle>
          <SheetDescription className='flex flex-wrap items-center gap-2'>
            <code className='bg-muted rounded px-1.5 py-0.5 font-mono text-xs'>{target?.maskedKey}</code>
            <span className='font-mono text-xs'>{target?.id}</span>
          </SheetDescription>
        </SheetHeader>

        <form
          id='credential-policy-form'
          onSubmit={handleSave}
          className='min-h-0 flex-1 space-y-5 overflow-y-auto px-4 py-2'
        >
          {/* Identity & Scheduling */}
          <div className='space-y-3.5'>
            <div className='space-y-1.5'>
              <Label htmlFor='cred-policy-name'>{t('channels.dialogs.keys.policy.fields.name')}</Label>
              <Input
                id='cred-policy-name'
                value={name}
                onChange={(event) => setName(event.target.value)}
                placeholder={t('channels.dialogs.keys.policy.fields.namePlaceholder')}
                maxLength={200}
              />
            </div>

            <div className='grid gap-3 sm:grid-cols-3'>
              <div className='space-y-1.5'>
                <Label htmlFor='cred-policy-priority'>{t('channels.dialogs.keys.policy.fields.priority')}</Label>
                <Input
                  id='cred-policy-priority'
                  type='number'
                  value={priority}
                  onChange={(event) => setPriority(event.target.value)}
                />
              </div>

              <div className='space-y-1.5'>
                <Label htmlFor='cred-policy-weight'>{t('channels.dialogs.keys.policy.fields.weight')}</Label>
                <Input
                  id='cred-policy-weight'
                  type='number'
                  min={1}
                  max={1000000}
                  value={weight}
                  onChange={(event) => setWeight(event.target.value)}
                />
              </div>

              <div className='space-y-1.5'>
                <Label htmlFor='cred-policy-max-concurrent'>
                  {t('channels.dialogs.keys.policy.fields.maxConcurrent')}
                </Label>
                <Input
                  id='cred-policy-max-concurrent'
                  type='number'
                  min={1}
                  value={maxConcurrent}
                  onChange={(event) => setMaxConcurrent(event.target.value)}
                  placeholder={t('channels.dialogs.keys.policy.fields.unlimited')}
                />
              </div>
            </div>
            <p className='text-muted-foreground text-xs'>
              {t('channels.dialogs.keys.policy.fields.schedulingHint')}
            </p>
          </div>

          {/* Outbound Egress */}
          <div className='space-y-3.5 rounded-lg border p-3.5'>
            <div className='flex items-center gap-1.5 text-sm font-medium'>
              <Shield className='text-muted-foreground h-4 w-4' />
              <span>{t('channels.dialogs.keys.policy.egressSection')}</span>
            </div>

            <div className='space-y-1.5'>
              <Label htmlFor='cred-policy-egress-mode'>
                {t('channels.dialogs.keys.policy.fields.egressMode')}
              </Label>
              <Select value={egressMode} onValueChange={(val) => setEgressMode(val as CredentialEgressMode)}>
                <SelectTrigger id='cred-policy-egress-mode'>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {EGRESS_MODES.map((mode) => (
                    <SelectItem key={mode} value={mode}>
                      {t(`channels.dialogs.keys.policy.egressModes.${mode}`)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            {egressMode === 'pinned_proxy' ? (
              <div className='space-y-1.5'>
                <Label htmlFor='cred-policy-proxy-id'>
                  {t('channels.dialogs.keys.policy.fields.outboundProxy')}
                </Label>
                <Select value={outboundProxyId} onValueChange={setOutboundProxyId}>
                  <SelectTrigger id='cred-policy-proxy-id'>
                    <SelectValue placeholder={t('channels.dialogs.keys.policy.fields.outboundProxyPlaceholder')} />
                  </SelectTrigger>
                  <SelectContent>
                    {outboundProxies.map((proxy) => {
                      const numId = extractNumberIDAsNumber(proxy.id);
                      const cap =
                        proxy.maxCredentials != null
                          ? `${proxy.credentialReferenceCount}/${proxy.maxCredentials}`
                          : `${proxy.credentialReferenceCount}`;
                      return (
                        <SelectItem key={proxy.id} value={String(numId)}>
                          {proxy.name} ({cap}) · {proxy.maskedUrl}
                          {proxy.status === 'disabled' ? ` [${t('system.outboundProxies.status.disabled')}]` : ''}
                        </SelectItem>
                      );
                    })}
                  </SelectContent>
                </Select>
                {selectedManagedProxy ? (
                  <div className='text-muted-foreground text-xs'>
                    {selectedBackupProxy
                      ? t('channels.dialogs.keys.policy.fields.backupProxyConfigured', {
                          name: selectedBackupProxy.name,
                        })
                      : t('channels.dialogs.keys.policy.fields.noBackupProxyConfigured')}
                  </div>
                ) : outboundProxies.length === 0 ? (
                  <p className='text-muted-foreground text-xs'>
                    {t('channels.dialogs.keys.policy.fields.noManagedProxiesHint')}
                  </p>
                ) : null}
              </div>
            ) : null}

            {egressMode === 'proxy_pool' ? (
              <div className='space-y-1.5'>
                <Label htmlFor='cred-policy-pool-name'>
                  {t('channels.dialogs.keys.policy.fields.proxyPool')}
                </Label>
                {poolOptions.length > 0 ? (
                  <Select value={proxyPoolName} onValueChange={setProxyPoolName}>
                    <SelectTrigger id='cred-policy-pool-name'>
                      <SelectValue placeholder={t('channels.dialogs.keys.policy.fields.proxyPoolPlaceholder')} />
                    </SelectTrigger>
                    <SelectContent>
                      {poolOptions.map((pool) => (
                        <SelectItem key={pool.name} value={pool.name}>
                          {pool.name} ({pool.enabledProxyCount}/{pool.proxyCount})
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                ) : (
                  <Input
                    id='cred-policy-pool-name'
                    value={proxyPoolName}
                    onChange={(event) => setProxyPoolName(event.target.value)}
                    placeholder='us-residential'
                  />
                )}
              </div>
            ) : null}

            {egressMode === 'custom_url' ? (
              <div className='space-y-3'>
                <div className='space-y-1.5'>
                  <Label htmlFor='cred-policy-custom-url'>
                    {t('channels.dialogs.keys.policy.fields.customProxyUrl')}
                  </Label>
                  <Input
                    id='cred-policy-custom-url'
                    className='font-mono text-sm'
                    value={customProxyUrl}
                    onChange={(event) => setCustomProxyUrl(event.target.value)}
                    placeholder='http://user:pass@proxy.example.com:8080'
                  />
                </div>
                <div className='grid gap-3 sm:grid-cols-2'>
                  <div className='space-y-1.5'>
                    <Label htmlFor='cred-policy-custom-user'>
                      {t('channels.dialogs.proxy.fields.username.label')}
                    </Label>
                    <Input
                      id='cred-policy-custom-user'
                      value={customProxyUsername}
                      onChange={(event) => setCustomProxyUsername(event.target.value)}
                    />
                  </div>
                  <div className='space-y-1.5'>
                    <Label htmlFor='cred-policy-custom-pass'>
                      {t('channels.dialogs.proxy.fields.password.label')}
                    </Label>
                    <Input
                      id='cred-policy-custom-pass'
                      type='password'
                      value={customProxyPassword}
                      onChange={(event) => setCustomProxyPassword(event.target.value)}
                    />
                  </div>
                </div>
                <div className='flex items-center justify-between rounded-md border px-3 py-2'>
                  <span className='text-xs font-medium'>
                    {t('channels.dialogs.proxy.fields.disableConnectionReuse.label')}
                  </span>
                  <Switch checked={customDisableReuse} onCheckedChange={setCustomDisableReuse} />
                </div>
              </div>
            ) : null}

            {showFailover ? (
              <div className='space-y-1.5 border-t pt-3'>
                <Label htmlFor='cred-policy-failover'>
                  {t('channels.dialogs.keys.policy.fields.egressFailPolicy')}
                </Label>
                <Select
                  value={egressFailPolicy}
                  onValueChange={(val) => setEgressFailPolicy(val as CredentialEgressFailPolicy)}
                >
                  <SelectTrigger id='cred-policy-failover'>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {FAIL_POLICIES.map((fp) => (
                      <SelectItem key={fp} value={fp}>
                        {t(`channels.dialogs.keys.policy.failPolicies.${fp}`)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className='text-muted-foreground text-xs'>
                  {t(`channels.dialogs.keys.policy.failPolicies.${egressFailPolicy}Hint`)}
                </p>
              </div>
            ) : null}
          </div>

          {/* Progressive Disclosure: Rate Interval & Manual Quota Cap */}
          <Collapsible open={limitsOpen} onOpenChange={setLimitsOpen} className='rounded-lg border'>
            <CollapsibleTrigger asChild>
              <button
                type='button'
                className='hover:bg-muted/40 flex w-full items-center justify-between px-3.5 py-2.5 text-left text-sm font-medium transition-colors'
              >
                <span>{t('channels.dialogs.keys.policy.advancedLimits')}</span>
                <ChevronDown className={cn('text-muted-foreground h-4 w-4 transition-transform', limitsOpen && 'rotate-180')} />
              </button>
            </CollapsibleTrigger>
            <CollapsibleContent className='space-y-3.5 border-t px-3.5 py-3.5'>
              <div className='space-y-1.5'>
                <Label htmlFor='cred-policy-interval'>
                  {t('channels.dialogs.keys.policy.fields.requestIntervalMs')}
                </Label>
                <Input
                  id='cred-policy-interval'
                  type='number'
                  min={0}
                  max={86400000}
                  value={requestIntervalMs}
                  onChange={(event) => setRequestIntervalMs(event.target.value)}
                  placeholder='0'
                />
              </div>
              <div className='space-y-1.5'>
                <Label htmlFor='cred-policy-manual-quota'>
                  {t('channels.dialogs.keys.policy.fields.manualQuotaLimit')}
                </Label>
                <Input
                  id='cred-policy-manual-quota'
                  type='number'
                  min={0}
                  step='any'
                  value={manualQuotaLimit}
                  onChange={(event) => setManualQuotaLimit(event.target.value)}
                  placeholder={t('channels.dialogs.keys.policy.fields.manualQuotaPlaceholder')}
                />
                <p className='text-muted-foreground text-xs'>
                  {t('channels.dialogs.keys.policy.fields.manualQuotaHint')}
                </p>
              </div>
            </CollapsibleContent>
          </Collapsible>

          {validationError ? <p className='text-destructive text-sm font-medium'>{validationError}</p> : null}
        </form>

        <SheetFooter className='border-t pt-3 sm:flex-row sm:justify-end'>
          <Button type='button' variant='outline' onClick={() => onOpenChange(false)} disabled={upsertPolicy.isPending}>
            {t('common.buttons.cancel')}
          </Button>
          <Button type='submit' form='credential-policy-form' disabled={upsertPolicy.isPending}>
            {upsertPolicy.isPending ? <Loader2 className='mr-2 h-4 w-4 animate-spin' /> : null}
            {upsertPolicy.isPending ? t('common.buttons.saving') : t('common.buttons.save')}
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}
