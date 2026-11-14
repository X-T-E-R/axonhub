'use client';

import { useMemo, useState } from 'react';
import {
  Activity,
  CheckCircle2,
  Loader2,
  Pencil,
  Plus,
  Search,
  Server,
  Trash2,
  XCircle,
} from 'lucide-react';
import { toast } from 'sonner';
import { useTranslation } from 'react-i18next';
import { cn, extractNumberIDAsNumber } from '@/lib/utils';
import { useDebounce } from '@/hooks/use-debounce';
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import {
  useDeleteOutboundProxy,
  useOutboundProxies,
  useOutboundProxyPools,
  useProbeOutboundProxy,
  useUpdateOutboundProxy,
  type OutboundProxy,
  type OutboundProxyStatus,
} from '../data/outbound-proxies';
import { OutboundProxyDialog } from './outbound-proxy-dialog';

type StatusFilter = 'all' | OutboundProxyStatus;

function isExpired(iso?: string | null): boolean {
  if (!iso) return false;
  const ts = Date.parse(iso);
  return Number.isFinite(ts) && ts <= Date.now();
}

function formatProbeTime(iso?: string | null, locale?: string): string {
  if (!iso) return '-';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  return new Intl.DateTimeFormat(locale || undefined, {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(date);
}

export function OutboundProxiesSettings() {
  const { t, i18n } = useTranslation();
  const { data: proxies = [], isLoading } = useOutboundProxies();
  const { data: pools = [] } = useOutboundProxyPools();
  const updateProxy = useUpdateOutboundProxy();
  const deleteProxy = useDeleteOutboundProxy();
  const probeProxy = useProbeOutboundProxy();

  const [selectedPool, setSelectedPool] = useState<string>('__all__');
  const [statusFilter, setStatusFilter] = useState<StatusFilter>('all');
  const [searchInput, setSearchInput] = useState('');
  const debouncedSearch = useDebounce(searchInput, 200);

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editingProxy, setEditingProxy] = useState<OutboundProxy | null>(null);
  const [deletingProxy, setDeletingProxy] = useState<OutboundProxy | null>(null);
  const [probingIds, setProbingIds] = useState<Set<string>>(new Set());
  const [probingAll, setProbingAll] = useState(false);

  const proxyByNumericId = useMemo(() => {
    const map = new Map<number, OutboundProxy>();
    for (const item of proxies) {
      map.set(extractNumberIDAsNumber(item.id), item);
    }
    return map;
  }, [proxies]);

  const enabledCount = useMemo(() => proxies.filter((item) => item.status === 'enabled').length, [proxies]);
  const standaloneCount = useMemo(() => proxies.filter((item) => !item.poolName).length, [proxies]);

  const filteredProxies = useMemo(() => {
    const query = debouncedSearch.trim().toLowerCase();
    return proxies.filter((item) => {
      if (selectedPool === '__standalone__' && item.poolName) return false;
      if (selectedPool !== '__all__' && selectedPool !== '__standalone__' && item.poolName !== selectedPool) {
        return false;
      }
      if (statusFilter !== 'all' && item.status !== statusFilter) {
        return false;
      }
      if (!query) return true;
      return (
        item.name.toLowerCase().includes(query) ||
        item.maskedUrl.toLowerCase().includes(query) ||
        item.poolName.toLowerCase().includes(query) ||
        (item.lastExitIp || '').toLowerCase().includes(query) ||
        (item.lastCountry || '').toLowerCase().includes(query) ||
        item.remark.toLowerCase().includes(query)
      );
    });
  }, [proxies, selectedPool, statusFilter, debouncedSearch]);

  const handleProbeSingle = async (proxy: OutboundProxy) => {
    setProbingIds((prev) => new Set(prev).add(proxy.id));
    try {
      await probeProxy.mutateAsync({ id: proxy.id });
    } catch {
      // Handled by hook.
    } finally {
      setProbingIds((prev) => {
        const next = new Set(prev);
        next.delete(proxy.id);
        return next;
      });
    }
  };

  const handleProbeVisible = async () => {
    const targets = filteredProxies.filter((item) => item.status === 'enabled');
    if (targets.length === 0) return;

    setProbingAll(true);
    let passed = 0;
    for (const target of targets) {
      setProbingIds((prev) => new Set(prev).add(target.id));
      try {
        const res = await probeProxy.mutateAsync({ id: target.id, silent: true });
        if (res.lastTestSuccess) {
          passed += 1;
        }
      } catch {
        // Counted as failed in batch summary.
      } finally {
        setProbingIds((prev) => {
          const next = new Set(prev);
          next.delete(target.id);
          return next;
        });
      }
    }
    setProbingAll(false);
    const failed = targets.length - passed;
    toast[failed === 0 ? 'success' : 'error'](
      t('system.outboundProxies.messages.probeBatchSummary', {
        passed,
        total: targets.length,
      })
    );
  };

  const handleStatusToggle = (proxy: OutboundProxy, checked: boolean) => {
    updateProxy.mutate({
      id: proxy.id,
      input: { status: checked ? 'enabled' : 'disabled' },
    });
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
    <>
      <Card>
        <CardHeader className='flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between'>
          <div className='space-y-1'>
            <CardTitle className='flex items-center gap-2'>
              <Server className='h-5 w-5' />
              {t('system.outboundProxies.title')}
            </CardTitle>
            <CardDescription>{t('system.outboundProxies.description')}</CardDescription>
          </div>
          <div className='flex shrink-0 flex-wrap items-center gap-2'>
            <Button
              type='button'
              variant='outline'
              size='sm'
              onClick={handleProbeVisible}
              disabled={probingAll || filteredProxies.filter((p) => p.status === 'enabled').length === 0}
            >
              {probingAll ? <Loader2 className='mr-1.5 h-4 w-4 animate-spin' /> : <Activity className='mr-1.5 h-4 w-4' />}
              {t('system.outboundProxies.actions.probeAll')}
            </Button>
            <Button
              type='button'
              size='sm'
              onClick={() => {
                setEditingProxy(null);
                setDialogOpen(true);
              }}
            >
              <Plus className='mr-1.5 h-4 w-4' />
              {t('system.outboundProxies.actions.add')}
            </Button>
          </div>
        </CardHeader>

        <CardContent className='space-y-4'>
          {/* Pool Summary Strip */}
          {proxies.length > 0 ? (
            <div className='flex flex-wrap items-center gap-2'>
              <button
                type='button'
                onClick={() => setSelectedPool('__all__')}
                className={cn(
                  'inline-flex items-center gap-2 rounded-lg border px-3 py-1.5 text-xs transition-colors',
                  selectedPool === '__all__'
                    ? 'border-primary bg-primary/10 text-foreground font-medium'
                    : 'bg-background hover:bg-muted/60 text-muted-foreground'
                )}
              >
                <span>{t('system.outboundProxies.pools.all')}</span>
                <span className='font-mono tabular-nums'>
                  {enabledCount}/{proxies.length}
                </span>
              </button>

              {pools.map((pool) => (
                <button
                  key={pool.name}
                  type='button'
                  onClick={() => setSelectedPool(selectedPool === pool.name ? '__all__' : pool.name)}
                  className={cn(
                    'inline-flex items-center gap-2 rounded-lg border px-3 py-1.5 text-xs transition-colors',
                    selectedPool === pool.name
                      ? 'border-primary bg-primary/10 text-foreground font-medium'
                      : 'bg-background hover:bg-muted/60 text-muted-foreground'
                  )}
                >
                  <span className='font-medium'>{pool.name}</span>
                  <span className='font-mono tabular-nums'>
                    {pool.enabledProxyCount}/{pool.proxyCount}
                  </span>
                  <span className='text-muted-foreground/80'>
                    · {t('system.outboundProxies.pools.boundCount', { count: pool.credentialReferenceCount })}
                  </span>
                </button>
              ))}

              {standaloneCount > 0 && pools.length > 0 ? (
                <button
                  type='button'
                  onClick={() => setSelectedPool(selectedPool === '__standalone__' ? '__all__' : '__standalone__')}
                  className={cn(
                    'inline-flex items-center gap-2 rounded-lg border px-3 py-1.5 text-xs transition-colors',
                    selectedPool === '__standalone__'
                      ? 'border-primary bg-primary/10 text-foreground font-medium'
                      : 'bg-background hover:bg-muted/60 text-muted-foreground'
                  )}
                >
                  <span>{t('system.outboundProxies.pools.standalone')}</span>
                  <span className='font-mono tabular-nums'>{standaloneCount}</span>
                </button>
              ) : null}
            </div>
          ) : null}

          {/* Search & Status Filter */}
          {proxies.length > 0 ? (
            <div className='flex flex-col gap-3 sm:flex-row sm:items-center'>
              <div className='relative flex-1'>
                <Search className='text-muted-foreground pointer-events-none absolute top-1/2 left-3 h-4 w-4 -translate-y-1/2' />
                <Input
                  type='search'
                  value={searchInput}
                  onChange={(event) => setSearchInput(event.target.value)}
                  placeholder={t('system.outboundProxies.filters.searchPlaceholder')}
                  className='pl-9'
                />
              </div>
              <Select value={statusFilter} onValueChange={(val) => setStatusFilter(val as StatusFilter)}>
                <SelectTrigger className='w-full sm:w-40'>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value='all'>{t('system.outboundProxies.filters.allStatuses')}</SelectItem>
                  <SelectItem value='enabled'>{t('system.outboundProxies.status.enabled')}</SelectItem>
                  <SelectItem value='disabled'>{t('system.outboundProxies.status.disabled')}</SelectItem>
                </SelectContent>
              </Select>
            </div>
          ) : null}

          {proxies.length === 0 ? (
            <div className='rounded-lg border border-dashed py-10 text-center'>
              <p className='text-muted-foreground text-sm'>{t('system.outboundProxies.empty')}</p>
            </div>
          ) : filteredProxies.length === 0 ? (
            <div className='rounded-lg border border-dashed py-8 text-center'>
              <p className='text-muted-foreground text-sm'>{t('system.outboundProxies.noMatches')}</p>
            </div>
          ) : (
            <>
              {/* Desktop Table */}
              <div className='hidden overflow-x-auto rounded-lg border md:block'>
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>{t('system.outboundProxies.columns.proxy')}</TableHead>
                      <TableHead>{t('system.outboundProxies.columns.pool')}</TableHead>
                      <TableHead>{t('system.outboundProxies.columns.capacity')}</TableHead>
                      <TableHead>{t('system.outboundProxies.columns.health')}</TableHead>
                      <TableHead>{t('system.outboundProxies.columns.status')}</TableHead>
                      <TableHead className='w-[132px] text-right'>{t('common.columns.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {filteredProxies.map((proxy) => {
                      const isProbing = probingIds.has(proxy.id);
                      const isReferenced = proxy.credentialReferenceCount > 0 || proxy.backupProxyCount > 0;
                      const expired = isExpired(proxy.expiresAt);
                      const atCapacity =
                        proxy.maxCredentials != null && proxy.credentialReferenceCount >= proxy.maxCredentials;
                      const backupNode =
                        proxy.backupProxyId != null ? proxyByNumericId.get(proxy.backupProxyId) : undefined;

                      return (
                        <TableRow key={proxy.id}>
                          <TableCell>
                            <div className='space-y-1'>
                              <div className='flex flex-wrap items-center gap-1.5'>
                                <span className='font-medium'>{proxy.name}</span>
                                <Badge variant={proxy.kind === 'dynamic_template' ? 'secondary' : 'outline'} className='text-[11px]'>
                                  {t(`system.outboundProxies.kind.${proxy.kind}`)}
                                </Badge>
                                {expired ? (
                                  <Badge variant='destructive' className='text-[11px]'>
                                    {t('system.outboundProxies.badges.expired')}
                                  </Badge>
                                ) : null}
                              </div>
                              <div className='text-muted-foreground font-mono text-xs break-all'>{proxy.maskedUrl}</div>
                              {(proxy.remark || backupNode) ? (
                                <div className='text-muted-foreground flex flex-wrap items-center gap-2 text-xs'>
                                  {backupNode ? (
                                    <span>
                                      {t('system.outboundProxies.badges.backupTo', { name: backupNode.name })}
                                    </span>
                                  ) : null}
                                  {proxy.remark ? <span className='max-w-56 truncate'>{proxy.remark}</span> : null}
                                </div>
                              ) : null}
                            </div>
                          </TableCell>

                          <TableCell>
                            {proxy.poolName ? (
                              <Badge variant='outline' className='font-mono text-xs'>
                                {proxy.poolName}
                              </Badge>
                            ) : (
                              <span className='text-muted-foreground text-xs'>-</span>
                            )}
                          </TableCell>

                          <TableCell>
                            <Tooltip>
                              <TooltipTrigger asChild>
                                <div className='inline-flex flex-col gap-0.5'>
                                  <div
                                    className={cn(
                                      'font-mono text-sm tabular-nums',
                                      atCapacity ? 'text-amber-600 dark:text-amber-400 font-medium' : ''
                                    )}
                                  >
                                    {proxy.maxCredentials != null
                                      ? `${proxy.credentialReferenceCount} / ${proxy.maxCredentials}`
                                      : String(proxy.credentialReferenceCount)}
                                  </div>
                                  <div className='text-muted-foreground text-[11px]'>
                                    {proxy.maxConcurrent != null
                                      ? t('system.outboundProxies.capacity.concurrentLimit', { count: proxy.maxConcurrent })
                                      : t('system.outboundProxies.capacity.weight', { weight: proxy.weight })}
                                  </div>
                                </div>
                              </TooltipTrigger>
                              <TooltipContent className='space-y-1 text-xs'>
                                <div>
                                  {t('system.outboundProxies.capacity.primaryAndBackup', {
                                    primary: proxy.primaryCredentialCount,
                                    backup: proxy.backupCredentialCount,
                                    channels: proxy.channelReferenceCount,
                                  })}
                                </div>
                              </TooltipContent>
                            </Tooltip>
                          </TableCell>

                          <TableCell>
                            {proxy.lastTestedAt == null ? (
                              <span className='text-muted-foreground text-xs'>
                                {t('system.outboundProxies.health.untested')}
                              </span>
                            ) : proxy.lastTestSuccess ? (
                              <div className='space-y-0.5'>
                                <div className='flex items-center gap-1.5 text-xs font-medium text-emerald-600 dark:text-emerald-400'>
                                  <CheckCircle2 className='h-3.5 w-3.5 shrink-0' />
                                  <span className='font-mono'>
                                    {proxy.lastExitIp || '-'}
                                    {proxy.lastCountry ? ` · ${proxy.lastCountry}` : ''}
                                  </span>
                                  {proxy.lastLatencyMs != null ? (
                                    <span className='text-muted-foreground font-mono font-normal'>
                                      {proxy.lastLatencyMs} ms
                                    </span>
                                  ) : null}
                                </div>
                                <div className='text-muted-foreground text-[11px]'>
                                  {formatProbeTime(proxy.lastTestedAt, i18n.language)}
                                </div>
                              </div>
                            ) : (
                              <Tooltip>
                                <TooltipTrigger asChild>
                                  <div className='space-y-0.5'>
                                    <div className='text-destructive flex items-center gap-1.5 text-xs font-medium'>
                                      <XCircle className='h-3.5 w-3.5 shrink-0' />
                                      <span className='max-w-44 truncate'>
                                        {proxy.lastTestError || t('system.outboundProxies.health.failed')}
                                      </span>
                                    </div>
                                    <div className='text-muted-foreground text-[11px]'>
                                      {formatProbeTime(proxy.lastTestedAt, i18n.language)}
                                    </div>
                                  </div>
                                </TooltipTrigger>
                                {proxy.lastTestError ? (
                                  <TooltipContent className='max-w-xs break-words text-xs'>
                                    {proxy.lastTestError}
                                  </TooltipContent>
                                ) : null}
                              </Tooltip>
                            )}
                          </TableCell>

                          <TableCell>
                            <Switch
                              checked={proxy.status === 'enabled'}
                              onCheckedChange={(checked) => handleStatusToggle(proxy, checked)}
                              disabled={updateProxy.isPending}
                              aria-label={t('system.outboundProxies.fields.status')}
                            />
                          </TableCell>

                          <TableCell className='text-right'>
                            <div className='flex justify-end gap-1'>
                              <Tooltip>
                                <TooltipTrigger asChild>
                                  <Button
                                    type='button'
                                    variant='ghost'
                                    size='sm'
                                    className='h-8 w-8 p-0'
                                    onClick={() => handleProbeSingle(proxy)}
                                    disabled={isProbing}
                                    aria-label={t('system.outboundProxies.actions.probe')}
                                  >
                                    {isProbing ? (
                                      <Loader2 className='h-4 w-4 animate-spin' />
                                    ) : (
                                      <Activity className='h-4 w-4' />
                                    )}
                                  </Button>
                                </TooltipTrigger>
                                <TooltipContent>{t('system.outboundProxies.actions.probe')}</TooltipContent>
                              </Tooltip>

                              <Tooltip>
                                <TooltipTrigger asChild>
                                  <Button
                                    type='button'
                                    variant='ghost'
                                    size='sm'
                                    className='h-8 w-8 p-0'
                                    onClick={() => {
                                      setEditingProxy(proxy);
                                      setDialogOpen(true);
                                    }}
                                    aria-label={t('common.buttons.edit')}
                                  >
                                    <Pencil className='h-4 w-4' />
                                  </Button>
                                </TooltipTrigger>
                                <TooltipContent>{t('common.buttons.edit')}</TooltipContent>
                              </Tooltip>

                              <Tooltip>
                                <TooltipTrigger asChild>
                                  <span>
                                    <Button
                                      type='button'
                                      variant='ghost'
                                      size='sm'
                                      className='hover:text-destructive h-8 w-8 p-0'
                                      onClick={() => setDeletingProxy(proxy)}
                                      disabled={isReferenced || deleteProxy.isPending}
                                      aria-label={t('common.buttons.delete')}
                                    >
                                      <Trash2 className='h-4 w-4' />
                                    </Button>
                                  </span>
                                </TooltipTrigger>
                                <TooltipContent>
                                  {isReferenced
                                    ? t('system.outboundProxies.delete.referencedHint')
                                    : t('common.buttons.delete')}
                                </TooltipContent>
                              </Tooltip>
                            </div>
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>

              {/* Mobile Cards */}
              <div className='space-y-3 md:hidden'>
                {filteredProxies.map((proxy) => {
                  const isProbing = probingIds.has(proxy.id);
                  const isReferenced = proxy.credentialReferenceCount > 0 || proxy.backupProxyCount > 0;
                  return (
                    <div key={proxy.id} className='space-y-2.5 rounded-lg border p-3.5'>
                      <div className='flex items-start justify-between gap-2'>
                        <div className='min-w-0 flex-1'>
                          <div className='flex flex-wrap items-center gap-1.5'>
                            <span className='truncate text-sm font-medium'>{proxy.name}</span>
                            <Badge variant='outline' className='text-[11px]'>
                              {t(`system.outboundProxies.kind.${proxy.kind}`)}
                            </Badge>
                            {proxy.poolName ? (
                              <Badge variant='secondary' className='font-mono text-[11px]'>
                                {proxy.poolName}
                              </Badge>
                            ) : null}
                          </div>
                          <div className='text-muted-foreground mt-1 font-mono text-xs break-all'>{proxy.maskedUrl}</div>
                        </div>
                        <Switch
                          checked={proxy.status === 'enabled'}
                          onCheckedChange={(checked) => handleStatusToggle(proxy, checked)}
                          disabled={updateProxy.isPending}
                        />
                      </div>

                      <div className='text-muted-foreground flex flex-wrap items-center justify-between gap-2 text-xs'>
                        <span className='font-mono'>
                          {t('system.outboundProxies.columns.capacity')}:{' '}
                          {proxy.maxCredentials != null
                            ? `${proxy.credentialReferenceCount}/${proxy.maxCredentials}`
                            : proxy.credentialReferenceCount}
                        </span>
                        <span>
                          {proxy.lastTestedAt == null
                            ? t('system.outboundProxies.health.untested')
                            : proxy.lastTestSuccess
                              ? `${proxy.lastExitIp || '-'} (${proxy.lastLatencyMs ?? 0} ms)`
                              : t('system.outboundProxies.health.failed')}
                        </span>
                      </div>

                      <div className='flex justify-end gap-1 border-t pt-2'>
                        <Button
                          type='button'
                          variant='ghost'
                          size='sm'
                          onClick={() => handleProbeSingle(proxy)}
                          disabled={isProbing}
                        >
                          {isProbing ? <Loader2 className='mr-1.5 h-3.5 w-3.5 animate-spin' /> : <Activity className='mr-1.5 h-3.5 w-3.5' />}
                          {t('system.outboundProxies.actions.probe')}
                        </Button>
                        <Button
                          type='button'
                          variant='ghost'
                          size='sm'
                          onClick={() => {
                            setEditingProxy(proxy);
                            setDialogOpen(true);
                          }}
                        >
                          <Pencil className='mr-1.5 h-3.5 w-3.5' />
                          {t('common.buttons.edit')}
                        </Button>
                        <Button
                          type='button'
                          variant='ghost'
                          size='sm'
                          className='text-destructive'
                          onClick={() => setDeletingProxy(proxy)}
                          disabled={isReferenced || deleteProxy.isPending}
                        >
                          <Trash2 className='mr-1.5 h-3.5 w-3.5' />
                          {t('common.buttons.delete')}
                        </Button>
                      </div>
                    </div>
                  );
                })}
              </div>
            </>
          )}
        </CardContent>
      </Card>

      <OutboundProxyDialog
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        proxy={editingProxy}
        proxies={proxies}
        pools={pools}
      />

      <AlertDialog open={Boolean(deletingProxy)} onOpenChange={(state) => !state && setDeletingProxy(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t('system.outboundProxies.delete.title')}</AlertDialogTitle>
            <AlertDialogDescription>
              {t('system.outboundProxies.delete.description', { name: deletingProxy?.name || '' })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t('common.buttons.cancel')}</AlertDialogCancel>
            <AlertDialogAction
              className='bg-destructive text-destructive-foreground hover:bg-destructive/90'
              disabled={deleteProxy.isPending}
              onClick={async (event) => {
                event.preventDefault();
                if (!deletingProxy) return;
                try {
                  await deleteProxy.mutateAsync(deletingProxy.id);
                  setDeletingProxy(null);
                } catch {
                  // Handled by hook.
                }
              }}
            >
              {deleteProxy.isPending ? <Loader2 className='mr-2 h-4 w-4 animate-spin' /> : null}
              {t('common.buttons.delete')}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
