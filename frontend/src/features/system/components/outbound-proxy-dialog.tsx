'use client';

import { useEffect, useMemo, useState } from 'react';
import { ChevronDown, Loader2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { cn, extractNumberIDAsNumber } from '@/lib/utils';
import { Button } from '@/components/ui/button';
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import {
  useCreateOutboundProxy,
  useUpdateOutboundProxy,
  type OutboundProxy,
  type OutboundProxyKind,
  type OutboundProxyPool,
  type OutboundProxyStatus,
} from '../data/outbound-proxies';

interface OutboundProxyDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  proxy?: OutboundProxy | null;
  proxies: OutboundProxy[];
  pools: OutboundProxyPool[];
}

function toDatetimeLocalValue(iso?: string | null): string {
  if (!iso) return '';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function parseOptionalPositiveInt(raw: string): { value?: number; valid: boolean } {
  const trimmed = raw.trim();
  if (!trimmed) return { value: undefined, valid: true };
  const parsed = Number(trimmed);
  if (!Number.isInteger(parsed) || parsed < 1) {
    return { valid: false };
  }
  return { value: parsed, valid: true };
}

export function OutboundProxyDialog({ open, onOpenChange, proxy, proxies, pools }: OutboundProxyDialogProps) {
  const { t } = useTranslation();
  const createProxy = useCreateOutboundProxy();
  const updateProxy = useUpdateOutboundProxy();
  const isEditing = Boolean(proxy);

  const [name, setName] = useState('');
  const [kind, setKind] = useState<OutboundProxyKind>('static');
  const [proxyUrl, setProxyUrl] = useState('');
  const [poolName, setPoolName] = useState('');
  const [status, setStatus] = useState<OutboundProxyStatus>('enabled');
  const [maxCredentials, setMaxCredentials] = useState('');
  const [weight, setWeight] = useState('100');
  const [maxConcurrent, setMaxConcurrent] = useState('');
  const [backupProxyId, setBackupProxyId] = useState<string>('none');
  const [expiresAt, setExpiresAt] = useState('');
  const [remark, setRemark] = useState('');
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [validationError, setValidationError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    setValidationError(null);
    if (proxy) {
      setName(proxy.name);
      setKind(proxy.kind);
      setProxyUrl('');
      setPoolName(proxy.poolName || '');
      setStatus(proxy.status);
      setMaxCredentials(proxy.maxCredentials != null ? String(proxy.maxCredentials) : '');
      setWeight(String(proxy.weight ?? 100));
      setMaxConcurrent(proxy.maxConcurrent != null ? String(proxy.maxConcurrent) : '');
      setBackupProxyId(proxy.backupProxyId != null ? String(proxy.backupProxyId) : 'none');
      setExpiresAt(toDatetimeLocalValue(proxy.expiresAt));
      setRemark(proxy.remark || '');
      setAdvancedOpen(
        (proxy.weight != null && proxy.weight !== 100) ||
          proxy.maxConcurrent != null ||
          proxy.backupProxyId != null ||
          Boolean(proxy.expiresAt) ||
          Boolean(proxy.remark)
      );
    } else {
      setName('');
      setKind('static');
      setProxyUrl('');
      setPoolName('');
      setStatus('enabled');
      setMaxCredentials('');
      setWeight('100');
      setMaxConcurrent('');
      setBackupProxyId('none');
      setExpiresAt('');
      setRemark('');
      setAdvancedOpen(false);
    }
  }, [open, proxy]);

  const currentNumericId = proxy ? extractNumberIDAsNumber(proxy.id) : null;
  const backupCandidates = useMemo(
    () => proxies.filter((candidate) => extractNumberIDAsNumber(candidate.id) !== currentNumericId),
    [proxies, currentNumericId]
  );

  const isPending = createProxy.isPending || updateProxy.isPending;

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault();
    setValidationError(null);

    const trimmedName = name.trim();
    const trimmedUrl = proxyUrl.trim();
    if (!trimmedName) {
      setValidationError(t('system.outboundProxies.validation.nameRequired'));
      return;
    }
    if (!isEditing && !trimmedUrl) {
      setValidationError(t('system.outboundProxies.validation.urlRequired'));
      return;
    }
    if (trimmedUrl && kind === 'static' && /\{[^}]*\}/.test(trimmedUrl)) {
      setValidationError(t('system.outboundProxies.validation.staticHasPlaceholders'));
      return;
    }

    const parsedMaxCredentials = parseOptionalPositiveInt(maxCredentials);
    if (!parsedMaxCredentials.valid) {
      setValidationError(t('system.outboundProxies.validation.invalidMaxCredentials'));
      return;
    }

    const parsedMaxConcurrent = parseOptionalPositiveInt(maxConcurrent);
    if (!parsedMaxConcurrent.valid) {
      setValidationError(t('system.outboundProxies.validation.invalidMaxConcurrent'));
      return;
    }

    const parsedWeight = parseOptionalPositiveInt(weight);
    if (!parsedWeight.valid || parsedWeight.value == null) {
      setValidationError(t('system.outboundProxies.validation.invalidWeight'));
      return;
    }

    const resolvedBackupId = backupProxyId !== 'none' ? Number(backupProxyId) : undefined;
    const resolvedExpiresAt = expiresAt.trim() ? new Date(expiresAt).toISOString() : undefined;

    try {
      if (isEditing && proxy) {
        await updateProxy.mutateAsync({
          id: proxy.id,
          input: {
            name: trimmedName,
            kind,
            ...(trimmedUrl ? { proxyUrl: trimmedUrl } : {}),
            poolName: poolName.trim(),
            status,
            weight: parsedWeight.value,
            ...(parsedMaxCredentials.value != null
              ? { maxCredentials: parsedMaxCredentials.value }
              : proxy.maxCredentials != null
                ? { clearMaxCredentials: true }
                : {}),
            ...(parsedMaxConcurrent.value != null
              ? { maxConcurrent: parsedMaxConcurrent.value }
              : proxy.maxConcurrent != null
                ? { clearMaxConcurrent: true }
                : {}),
            ...(resolvedBackupId != null
              ? { backupProxyId: resolvedBackupId }
              : proxy.backupProxyId != null
                ? { clearBackupProxyId: true }
                : {}),
            ...(resolvedExpiresAt
              ? { expiresAt: resolvedExpiresAt }
              : proxy.expiresAt
                ? { clearExpiresAt: true }
                : {}),
            remark: remark.trim(),
          },
        });
      } else {
        await createProxy.mutateAsync({
          name: trimmedName,
          kind,
          proxyUrl: trimmedUrl,
          poolName: poolName.trim() || undefined,
          status,
          weight: parsedWeight.value,
          ...(parsedMaxCredentials.value != null ? { maxCredentials: parsedMaxCredentials.value } : {}),
          ...(parsedMaxConcurrent.value != null ? { maxConcurrent: parsedMaxConcurrent.value } : {}),
          ...(resolvedBackupId != null ? { backupProxyId: resolvedBackupId } : {}),
          ...(resolvedExpiresAt ? { expiresAt: resolvedExpiresAt } : {}),
          remark: remark.trim() || undefined,
        });
      }
      onOpenChange(false);
    } catch {
      // Handled by mutation error handler.
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='flex max-h-[90vh] flex-col sm:max-w-xl'>
        <DialogHeader className='text-left'>
          <DialogTitle>
            {isEditing ? t('system.outboundProxies.dialog.editTitle') : t('system.outboundProxies.dialog.createTitle')}
          </DialogTitle>
          <DialogDescription>
            {isEditing
              ? t('system.outboundProxies.dialog.editDescription')
              : t('system.outboundProxies.dialog.createDescription')}
          </DialogDescription>
        </DialogHeader>

        <form id='outbound-proxy-form' onSubmit={handleSubmit} className='min-h-0 flex-1 space-y-4 overflow-y-auto pr-1'>
          <div className='grid gap-4 sm:grid-cols-2'>
            <div className='space-y-1.5'>
              <Label htmlFor='outbound-proxy-name'>{t('system.outboundProxies.fields.name')}</Label>
              <Input
                id='outbound-proxy-name'
                value={name}
                onChange={(event) => setName(event.target.value)}
                placeholder={t('system.outboundProxies.fields.namePlaceholder')}
                required
              />
            </div>

            <div className='space-y-1.5'>
              <Label htmlFor='outbound-proxy-status'>{t('system.outboundProxies.fields.status')}</Label>
              <Select value={status} onValueChange={(val) => setStatus(val as OutboundProxyStatus)}>
                <SelectTrigger id='outbound-proxy-status'>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value='enabled'>{t('system.outboundProxies.status.enabled')}</SelectItem>
                  <SelectItem value='disabled'>{t('system.outboundProxies.status.disabled')}</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className='grid gap-4 sm:grid-cols-2'>
            <div className='space-y-1.5'>
              <Label htmlFor='outbound-proxy-kind'>{t('system.outboundProxies.fields.kind')}</Label>
              <Select value={kind} onValueChange={(val) => setKind(val as OutboundProxyKind)}>
                <SelectTrigger id='outbound-proxy-kind'>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value='static'>{t('system.outboundProxies.kind.static')}</SelectItem>
                  <SelectItem value='dynamic_template'>{t('system.outboundProxies.kind.dynamic_template')}</SelectItem>
                </SelectContent>
              </Select>
            </div>

            <div className='space-y-1.5'>
              <Label htmlFor='outbound-proxy-pool'>{t('system.outboundProxies.fields.poolName')}</Label>
              <Input
                id='outbound-proxy-pool'
                list='outbound-proxy-pool-suggestions'
                value={poolName}
                onChange={(event) => setPoolName(event.target.value)}
                placeholder={t('system.outboundProxies.fields.poolPlaceholder')}
              />
              <datalist id='outbound-proxy-pool-suggestions'>
                {pools.map((pool) => (
                  <option key={pool.name} value={pool.name} />
                ))}
              </datalist>
            </div>
          </div>

          <div className='space-y-1.5'>
            <div className='flex items-center justify-between gap-2'>
              <Label htmlFor='outbound-proxy-url'>{t('system.outboundProxies.fields.proxyUrl')}</Label>
              {isEditing && proxy?.maskedUrl ? (
                <code className='bg-muted text-muted-foreground max-w-[60%] truncate rounded px-1.5 py-0.5 font-mono text-xs'>
                  {proxy.maskedUrl}
                </code>
              ) : null}
            </div>
            <Input
              id='outbound-proxy-url'
              type='text'
              autoComplete='off'
              spellCheck={false}
              className='font-mono text-sm'
              value={proxyUrl}
              onChange={(event) => {
                const nextUrl = event.target.value;
                setProxyUrl(nextUrl);
                if (kind === 'static' && /(\{credential_id\}|\{session\}|\{channel_id\})/.test(nextUrl)) {
                  setKind('dynamic_template');
                }
              }}
              placeholder={
                isEditing
                  ? t('system.outboundProxies.fields.proxyUrlEditPlaceholder')
                  : kind === 'dynamic_template'
                    ? 'http://user-{session}:pass@gw.example.com:8080'
                    : 'http://user:pass@proxy.example.com:8080'
              }
              required={!isEditing}
            />
            {kind === 'dynamic_template' ? (
              <p className='text-muted-foreground text-xs'>{t('system.outboundProxies.fields.templateHint')}</p>
            ) : isEditing ? (
              <p className='text-muted-foreground text-xs'>{t('system.outboundProxies.fields.rotateUrlHint')}</p>
            ) : null}
          </div>

          <div className='space-y-1.5'>
            <Label htmlFor='outbound-proxy-max-credentials'>{t('system.outboundProxies.fields.maxCredentials')}</Label>
            <Input
              id='outbound-proxy-max-credentials'
              type='number'
              min={1}
              value={maxCredentials}
              onChange={(event) => setMaxCredentials(event.target.value)}
              placeholder={t('system.outboundProxies.fields.maxCredentialsPlaceholder')}
            />
            <p className='text-muted-foreground text-xs'>{t('system.outboundProxies.fields.maxCredentialsHint')}</p>
          </div>

          <Collapsible open={advancedOpen} onOpenChange={setAdvancedOpen} className='rounded-lg border'>
            <CollapsibleTrigger asChild>
              <button
                type='button'
                className='hover:bg-muted/40 flex w-full items-center justify-between px-3.5 py-2.5 text-left text-sm font-medium transition-colors'
              >
                <span>{t('system.outboundProxies.dialog.advancedTitle')}</span>
                <ChevronDown className={cn('text-muted-foreground h-4 w-4 transition-transform', advancedOpen && 'rotate-180')} />
              </button>
            </CollapsibleTrigger>
            <CollapsibleContent className='space-y-4 border-t px-3.5 py-3.5'>
              <div className='grid gap-4 sm:grid-cols-2'>
                <div className='space-y-1.5'>
                  <Label htmlFor='outbound-proxy-weight'>{t('system.outboundProxies.fields.weight')}</Label>
                  <Input
                    id='outbound-proxy-weight'
                    type='number'
                    min={1}
                    value={weight}
                    onChange={(event) => setWeight(event.target.value)}
                  />
                </div>

                <div className='space-y-1.5'>
                  <Label htmlFor='outbound-proxy-max-concurrent'>{t('system.outboundProxies.fields.maxConcurrent')}</Label>
                  <Input
                    id='outbound-proxy-max-concurrent'
                    type='number'
                    min={1}
                    value={maxConcurrent}
                    onChange={(event) => setMaxConcurrent(event.target.value)}
                    placeholder={t('system.outboundProxies.fields.unlimitedPlaceholder')}
                  />
                </div>
              </div>

              <div className='grid gap-4 sm:grid-cols-2'>
                <div className='space-y-1.5'>
                  <Label htmlFor='outbound-proxy-backup'>{t('system.outboundProxies.fields.backupProxy')}</Label>
                  <Select value={backupProxyId} onValueChange={setBackupProxyId}>
                    <SelectTrigger id='outbound-proxy-backup'>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value='none'>{t('system.outboundProxies.fields.noBackup')}</SelectItem>
                      {backupCandidates.map((candidate) => {
                        const numId = extractNumberIDAsNumber(candidate.id);
                        return (
                          <SelectItem key={candidate.id} value={String(numId)}>
                            {candidate.name} ({candidate.maskedUrl})
                          </SelectItem>
                        );
                      })}
                    </SelectContent>
                  </Select>
                </div>

                <div className='space-y-1.5'>
                  <Label htmlFor='outbound-proxy-expires-at'>{t('system.outboundProxies.fields.expiresAt')}</Label>
                  <Input
                    id='outbound-proxy-expires-at'
                    type='datetime-local'
                    value={expiresAt}
                    onChange={(event) => setExpiresAt(event.target.value)}
                  />
                </div>
              </div>

              <div className='space-y-1.5'>
                <Label htmlFor='outbound-proxy-remark'>{t('system.outboundProxies.fields.remark')}</Label>
                <Input
                  id='outbound-proxy-remark'
                  value={remark}
                  onChange={(event) => setRemark(event.target.value)}
                  placeholder={t('system.outboundProxies.fields.remarkPlaceholder')}
                />
              </div>
            </CollapsibleContent>
          </Collapsible>

          {validationError ? <p className='text-destructive text-sm font-medium'>{validationError}</p> : null}
        </form>

        <DialogFooter className='shrink-0'>
          <Button type='button' variant='outline' onClick={() => onOpenChange(false)} disabled={isPending}>
            {t('common.buttons.cancel')}
          </Button>
          <Button type='submit' form='outbound-proxy-form' disabled={isPending}>
            {isPending ? <Loader2 className='mr-2 h-4 w-4 animate-spin' /> : null}
            {isPending ? t('common.buttons.saving') : t('common.buttons.save')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
