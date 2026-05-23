'use client';

import { useEffect, useMemo, useState } from 'react';
import { format } from 'date-fns';
import { z } from 'zod';
import { zodResolver } from '@hookform/resolvers/zod';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import {
  IconArchive,
  IconAlertTriangle,
  IconDatabase,
  IconKey,
  IconKeyOff,
  IconLoader2,
  IconPlayerPlay,
  IconRefresh,
  IconRestore,
  IconRoute,
  IconSettingsAutomation,
  IconTrash,
} from '@tabler/icons-react';
import { Alert, AlertDescription } from '@/components/ui/alert';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Checkbox } from '@/components/ui/checkbox';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { ScrollArea } from '@/components/ui/scroll-area';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Separator } from '@/components/ui/separator';
import { Switch } from '@/components/ui/switch';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { Textarea } from '@/components/ui/textarea';
import {
  useAddChannelAPIKey,
  useArchiveChannelAPIKey,
  useChannelAPIKeyInventory,
  useDeleteChannelAPIKey,
  useDisableChannelAPIKey,
  useEnableChannelAPIKey,
  useRestoreChannelAPIKey,
  useTestChannelAPIKeys,
  useUpdateChannel,
} from '../data/channels';
import {
  Channel,
  ChannelAPIKeyInventoryItem,
  ChannelKeyHealthCheck,
  channelKeyHealthCheckFailureActionSchema,
  channelKeySelectionStrategySchema,
} from '../data/schema';
import { mergeChannelSettingsForUpdate } from '../utils/merge';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  currentRow: Channel;
}

type KeyInventoryStatus = 'active' | 'disabled' | 'archived';

interface KeyInventoryRow {
  id: string;
  maskedKey: string;
  status: KeyInventoryStatus;
  lastCheckedAt?: string | null;
  failureCount?: number | null;
  balance?: unknown;
  currency?: string | null;
  available?: boolean | null;
  reason?: string | null;
}

const keysFormSchema = z.object({
  strategy: channelKeySelectionStrategySchema,
  newKey: z.string().optional(),
  healthCheck: z.object({
    enabled: z.boolean(),
    intervalMinutes: z.coerce.number().int().min(5).max(10080),
    failureThreshold: z.coerce.number().int().min(1).max(20),
    failureAction: channelKeyHealthCheckFailureActionSchema,
    includeDisabled: z.boolean(),
    builtinRuleEnabled: z.boolean(),
    deepseekRuleEnabled: z.boolean(),
    deepseekPath: z.string().min(1),
    deepseekExpectedStatuses: z.string(),
    deepseekPassWhen: z.string(),
  }),
});

type KeysFormInput = z.input<typeof keysFormSchema>;
type KeysFormValues = z.output<typeof keysFormSchema>;

const DEFAULT_STRATEGY: KeysFormValues['strategy'] = 'trace_sticky';
const STRATEGIES: KeysFormValues['strategy'][] = ['trace_sticky', 'cache_affinity', 'random', 'round_robin'];
const FAILURE_ACTIONS: KeysFormValues['healthCheck']['failureAction'][] = ['report_only', 'disable', 'archive', 'delete'];

const DEFAULT_HEALTH_CHECK: KeysFormValues['healthCheck'] = {
  enabled: false,
  intervalMinutes: 60,
  failureThreshold: 3,
  failureAction: 'report_only',
  includeDisabled: false,
  builtinRuleEnabled: true,
  deepseekRuleEnabled: false,
  deepseekPath: '/user/balance',
  deepseekExpectedStatuses: '200',
  deepseekPassWhen: 'json.is_available == true',
};

function parseStatusList(input: string): number[] {
  return input
    .split(',')
    .map((item) => Number(item.trim()))
    .filter((item) => Number.isInteger(item) && item >= 100 && item <= 599);
}

function valuesFromChannel(currentRow: Channel): KeysFormValues {
  const health = currentRow.settings?.keyHealthCheck;
  const rules = health?.rules ?? [];
  const builtinRule = rules.find((rule) => rule.type === 'builtin_test');
  const deepseekRule = rules.find((rule) => rule.type === 'http' && (rule.name.toLowerCase().includes('deepseek') || rule.http?.path === '/user/balance'));

  return {
    strategy: currentRow.settings?.keySelection?.strategy ?? DEFAULT_STRATEGY,
    newKey: '',
    healthCheck: {
      enabled: health?.enabled ?? DEFAULT_HEALTH_CHECK.enabled,
      intervalMinutes: health?.intervalMinutes ?? DEFAULT_HEALTH_CHECK.intervalMinutes,
      failureThreshold: health?.failureThreshold ?? DEFAULT_HEALTH_CHECK.failureThreshold,
      failureAction: health?.failureAction ?? DEFAULT_HEALTH_CHECK.failureAction,
      includeDisabled: health?.includeDisabled ?? DEFAULT_HEALTH_CHECK.includeDisabled,
      builtinRuleEnabled: builtinRule?.enabled ?? !!builtinRule ?? DEFAULT_HEALTH_CHECK.builtinRuleEnabled,
      deepseekRuleEnabled: deepseekRule?.enabled ?? !!deepseekRule ?? DEFAULT_HEALTH_CHECK.deepseekRuleEnabled,
      deepseekPath: deepseekRule?.http?.path || DEFAULT_HEALTH_CHECK.deepseekPath,
      deepseekExpectedStatuses: (deepseekRule?.http?.expectedStatuses ?? [200]).join(', '),
      deepseekPassWhen: deepseekRule?.http?.passWhen || DEFAULT_HEALTH_CHECK.deepseekPassWhen,
    },
  };
}

function healthCheckFromValues(values: KeysFormValues, existing?: ChannelKeyHealthCheck | null): ChannelKeyHealthCheck {
  const rules: ChannelKeyHealthCheck['rules'] = [];

  if (values.healthCheck.builtinRuleEnabled) {
    rules.push({
      id: 'builtin-key-test',
      name: 'Built-in key test',
      type: 'builtin_test',
      enabled: true,
      builtin: {
        kind: 'channel_api_key_test',
      },
    });
  }

  if (values.healthCheck.deepseekRuleEnabled) {
    rules.push({
      id: 'deepseek-balance',
      name: 'DeepSeek balance',
      type: 'http',
      enabled: true,
      http: {
        method: 'GET',
        urlMode: 'provider_base_url',
        path: values.healthCheck.deepseekPath,
        timeoutMs: 10000,
        headers: [],
        keyInjection: {
          location: 'authorization_bearer',
        },
        expectedStatuses: parseStatusList(values.healthCheck.deepseekExpectedStatuses),
        passWhen: values.healthCheck.deepseekPassWhen,
      },
    });
  }

  return {
    enabled: values.healthCheck.enabled,
    intervalMinutes: values.healthCheck.intervalMinutes,
    failureThreshold: values.healthCheck.failureThreshold,
    failureAction: values.healthCheck.failureAction,
    includeDisabled: values.healthCheck.includeDisabled,
    rules,
    keyMetadata: existing?.keyMetadata ?? null,
    archivedKeys: existing?.archivedKeys ?? null,
  };
}

function formatDateTime(value?: string | null): string {
  if (!value) {
    return '-';
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return format(date, 'yyyy-MM-dd HH:mm:ss');
}

function inventoryFromBackend(items: ChannelAPIKeyInventoryItem[] = []): KeyInventoryRow[] {
  return items.map((item) => ({
    id: item.id,
    maskedKey: item.maskedKey,
    status: item.status,
    lastCheckedAt: item.lastCheckedAt,
    failureCount: item.failureCount,
    balance: item.balance,
    currency: item.currency,
    available: item.available,
    reason: item.reason,
  }));
}

function mapTestResultsToInventory(
  results: Array<{ keyPrefix: string; success: boolean; latency: number; error?: string | null }>,
  inventory: KeyInventoryRow[]
): Record<string, { success: boolean; latency: number; error?: string | null }> {
  const buckets = new Map<string, KeyInventoryRow[]>();
  for (const item of inventory) {
    const rows = buckets.get(item.maskedKey) ?? [];
    rows.push(item);
    buckets.set(item.maskedKey, rows);
  }

  return results.reduce<Record<string, { success: boolean; latency: number; error?: string | null }>>((acc, result) => {
    const matched = buckets.get(result.keyPrefix)?.shift();
    if (matched) {
      acc[matched.id] = {
        success: result.success,
        latency: result.latency,
        error: result.error,
      };
    }
    return acc;
  }, {});
}

function formatBalance(value: unknown, currency?: string | null): string {
  if (value == null) {
    return '-';
  }
  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
    return `${value} ${currency ?? ''}`.trim();
  }
  if (Array.isArray(value)) {
    return `${value.length} items`;
  }
  if (typeof value === 'object') {
    const record = value as Record<string, unknown>;
    const candidates = ['total_balance', 'totalBalance', 'balance', 'available_balance', 'availableBalance'];
    for (const key of candidates) {
      const candidate = record[key];
      if (typeof candidate === 'string' || typeof candidate === 'number') {
        return `${candidate} ${currency ?? ''}`.trim();
      }
    }
  }

  return JSON.stringify(value);
}

export function ChannelsKeysDialog({ open, onOpenChange, currentRow }: Props) {
  const { t } = useTranslation();
  const [selectedKeys, setSelectedKeys] = useState<Set<string>>(new Set());
  const [testResults, setTestResults] = useState<Record<string, { success: boolean; latency: number; error?: string | null }>>({});
  const [confirmDeleteKey, setConfirmDeleteKey] = useState<string | null>(null);

  const keyInventory = useChannelAPIKeyInventory(currentRow.id, { enabled: open });
  const addAPIKey = useAddChannelAPIKey();
  const deleteAPIKey = useDeleteChannelAPIKey();
  const archiveAPIKey = useArchiveChannelAPIKey();
  const restoreAPIKey = useRestoreChannelAPIKey();
  const updateChannel = useUpdateChannel();
  const testAPIKeys = useTestChannelAPIKeys({ silent: true });
  const disableAPIKey = useDisableChannelAPIKey();
  const enableAPIKey = useEnableChannelAPIKey();

  const form = useForm<KeysFormInput, unknown, KeysFormValues>({
    resolver: zodResolver(keysFormSchema),
    defaultValues: valuesFromChannel(currentRow),
    mode: 'onChange',
  });

  useEffect(() => {
    if (open) {
      form.reset(valuesFromChannel(currentRow));
      setSelectedKeys(new Set());
      setTestResults({});
      setConfirmDeleteKey(null);
    }
  }, [open, currentRow, form]);

  const inventory = useMemo(() => inventoryFromBackend(keyInventory.data), [keyInventory.data]);
  const activeKeys = useMemo(() => inventory.filter((item) => item.status === 'active'), [inventory]);
  const disabledKeys = useMemo(() => inventory.filter((item) => item.status === 'disabled'), [inventory]);
  const archivedKeys = useMemo(() => inventory.filter((item) => item.status === 'archived'), [inventory]);
  const selectedActiveKeyIDs = useMemo(() => inventory.filter((item) => item.status === 'active' && selectedKeys.has(item.id)).map((item) => item.id), [inventory, selectedKeys]);
  const selectedStrategy = form.watch('strategy');
  const deepseekRuleEnabled = form.watch('healthCheck.deepseekRuleEnabled');
  const isPending =
    keyInventory.isFetching ||
    addAPIKey.isPending ||
    deleteAPIKey.isPending ||
    archiveAPIKey.isPending ||
    restoreAPIKey.isPending ||
    updateChannel.isPending ||
    testAPIKeys.isPending ||
    disableAPIKey.isPending ||
    enableAPIKey.isPending;

  const toggleSelected = (id: string, checked: boolean) => {
    setSelectedKeys((prev) => {
      const next = new Set(prev);
      if (checked) {
        next.add(id);
      } else {
        next.delete(id);
      }
      return next;
    });
  };

  const saveSettings = async (values: KeysFormValues) => {
    const nextSettings = mergeChannelSettingsForUpdate(currentRow.settings, {
      keySelection: {
        strategy: values.strategy,
      },
      keyHealthCheck: healthCheckFromValues(values, currentRow.settings?.keyHealthCheck),
    });

    await updateChannel.mutateAsync({
      id: currentRow.id,
      input: {
        settings: nextSettings,
      },
    });
  };

  const handleSaveSettings = async (values: KeysFormValues) => {
    try {
      await saveSettings(values);
      toast.success(t('channels.messages.updateSuccess'));
      onOpenChange(false);
    } catch {
      // Error handled by hook.
    }
  };

  const handleAddKey = async () => {
    const key = form.getValues('newKey')?.trim();
    if (!key) {
      return;
    }

    try {
      await addAPIKey.mutateAsync({ channelID: currentRow.id, key });
      form.setValue('newKey', '');
    } catch {
      // Error handled by hook.
    }
  };

  const handleDeleteKey = async (keyID: string) => {
    try {
      await deleteAPIKey.mutateAsync({ channelID: currentRow.id, keyID });
      setConfirmDeleteKey(null);
    } catch {
      // Error handled by hook.
    }
  };

  const handleDisableKey = async (keyID: string) => {
    try {
      await disableAPIKey.mutateAsync({ channelID: currentRow.id, key: keyID });
    } catch {
      // Error handled by hook.
    }
  };

  const handleEnableKey = async (keyID: string) => {
    try {
      await enableAPIKey.mutateAsync({ channelID: currentRow.id, key: keyID });
    } catch {
      // Error handled by hook.
    }
  };

  const handleArchiveKey = async (keyID: string) => {
    try {
      await archiveAPIKey.mutateAsync({ channelID: currentRow.id, keyID, reason: 'Manually archived by user' });
    } catch {
      // Error handled by hook.
    }
  };

  const handleRestoreKey = async (keyID: string) => {
    try {
      await restoreAPIKey.mutateAsync({ channelID: currentRow.id, keyID });
    } catch {
      // Error handled by hook.
    }
  };

  const handleRunChecks = async () => {
    try {
      const data = await testAPIKeys.mutateAsync({
        channelID: currentRow.id,
        modelID: currentRow.defaultTestModel || undefined,
      });
      setTestResults(mapTestResultsToInventory(data.results, inventory));
      toast[data.failedCount === 0 ? 'success' : 'error'](
        t('channels.dialogs.testAPIKeys.successSummary', { success: data.successCount, total: data.total })
      );
    } catch {
      setTestResults({});
    }
  };

  const handleBatchDisable = async () => {
    await Promise.all(selectedActiveKeyIDs.map((keyID) => disableAPIKey.mutateAsync({ channelID: currentRow.id, key: keyID })));
    setSelectedKeys(new Set());
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='flex max-h-[92vh] flex-col sm:max-w-5xl'>
        <DialogHeader className='text-left'>
          <DialogTitle className='flex items-center gap-2'>
            <IconKey className='h-5 w-5' />
            {t('channels.dialogs.keys.title')}
          </DialogTitle>
          <DialogDescription>{t('channels.dialogs.keys.description', { name: currentRow.name })}</DialogDescription>
        </DialogHeader>

        <Form {...form}>
          <Tabs defaultValue='inventory' className='min-h-0 flex-1'>
            <TabsList className='grid w-full grid-cols-3'>
              <TabsTrigger value='inventory'>{t('channels.dialogs.keys.tabs.inventory')}</TabsTrigger>
              <TabsTrigger value='routing'>{t('channels.dialogs.keys.tabs.routing')}</TabsTrigger>
              <TabsTrigger value='health'>{t('channels.dialogs.keys.tabs.health')}</TabsTrigger>
            </TabsList>

            <ScrollArea className='mt-4 h-[58vh] pr-3'>
              <TabsContent value='inventory' className='mt-0 space-y-4'>
                <div className='grid gap-3 md:grid-cols-3'>
                  <Card>
                    <CardHeader className='pb-2'>
                      <CardTitle className='text-sm'>{t('channels.dialogs.keys.summary.active')}</CardTitle>
                    </CardHeader>
                    <CardContent className='text-2xl font-semibold'>{activeKeys.length}</CardContent>
                  </Card>
                  <Card>
                    <CardHeader className='pb-2'>
                      <CardTitle className='text-sm'>{t('channels.dialogs.keys.summary.disabled')}</CardTitle>
                    </CardHeader>
                    <CardContent className='text-2xl font-semibold'>{disabledKeys.length}</CardContent>
                  </Card>
                  <Card>
                    <CardHeader className='pb-2'>
                      <CardTitle className='text-sm'>{t('channels.dialogs.keys.summary.archived')}</CardTitle>
                    </CardHeader>
                    <CardContent className='text-2xl font-semibold'>{archivedKeys.length}</CardContent>
                  </Card>
                </div>

                <Card>
                  <CardHeader>
                    <CardTitle className='flex items-center gap-2'>
                      <IconKey className='h-5 w-5' />
                      {t('channels.dialogs.keys.inventory.title')}
                    </CardTitle>
                    <CardDescription>{t('channels.dialogs.keys.inventory.description')}</CardDescription>
                  </CardHeader>
                  <CardContent className='space-y-4'>
                    <div className='flex flex-col gap-2 sm:flex-row'>
                      <FormField
                        control={form.control}
                        name='newKey'
                        render={({ field }) => (
                          <FormItem className='flex-1'>
                            <FormControl>
                              <Input
                                type='password'
                                autoComplete='off'
                                placeholder={t('channels.dialogs.keys.fields.newKey.placeholder')}
                                {...field}
                              />
                            </FormControl>
                            <FormDescription>{t('channels.dialogs.keys.fields.newKey.description')}</FormDescription>
                          </FormItem>
                        )}
                      />
                      <Button type='button' className='sm:mt-0' onClick={handleAddKey} disabled={isPending}>
                        {t('channels.dialogs.keys.actions.add')}
                      </Button>
                    </div>

                    {selectedActiveKeyIDs.length > 0 && (
                      <div className='flex items-center justify-between rounded-md border bg-muted/40 px-3 py-2'>
                        <span className='text-sm'>{t('channels.dialogs.keys.selectedCount', { count: selectedActiveKeyIDs.length })}</span>
                        <Button type='button' variant='outline' size='sm' onClick={handleBatchDisable} disabled={isPending}>
                          <IconKeyOff className='mr-2 h-4 w-4' />
                          {t('channels.dialogs.keys.actions.disableSelected')}
                        </Button>
                      </div>
                    )}

                    <div className='rounded-lg border'>
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead className='w-12'></TableHead>
                            <TableHead>{t('channels.dialogs.keys.columns.key')}</TableHead>
                            <TableHead>{t('channels.dialogs.keys.columns.status')}</TableHead>
                            <TableHead>{t('channels.dialogs.keys.columns.lastCheck')}</TableHead>
                            <TableHead>{t('channels.dialogs.keys.columns.balance')}</TableHead>
                            <TableHead className='text-right'>{t('common.columns.actions')}</TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {inventory.length === 0 ? (
                            <TableRow>
                              <TableCell colSpan={6} className='h-28 text-center text-sm text-muted-foreground'>
                                {t('channels.dialogs.keys.inventory.empty')}
                              </TableCell>
                            </TableRow>
                          ) : (
                            inventory.map((item) => {
                              const result = testResults[item.id];
                              return (
                                <TableRow key={item.id}>
                                  <TableCell>
                                    {item.status === 'active' ? (
                                      <Checkbox
                                        checked={selectedKeys.has(item.id)}
                                        onCheckedChange={(checked) => toggleSelected(item.id, checked === true)}
                                      />
                                    ) : null}
                                  </TableCell>
                                  <TableCell>
                                    <div className='flex flex-col gap-1'>
                                      <code className='w-fit rounded bg-muted px-2 py-0.5 font-mono text-sm'>{item.maskedKey}</code>
                                      <div className='flex flex-wrap items-center gap-2 text-xs text-muted-foreground'>
                                        {item.reason ? <span className='max-w-64 truncate'>{item.reason}</span> : null}
                                        {result ? (
                                          <span className={result.success ? 'text-green-600' : 'text-destructive'}>
                                            {result.success
                                              ? t('channels.dialogs.keys.checkResult.ok', { latency: result.latency.toFixed(2) })
                                              : result.error || t('channels.dialogs.keys.checkResult.failed')}
                                          </span>
                                        ) : null}
                                      </div>
                                    </div>
                                  </TableCell>
                                  <TableCell>
                                    <Badge
                                      variant={item.status === 'active' ? 'default' : item.status === 'disabled' ? 'secondary' : 'outline'}
                                    >
                                      {t(`channels.dialogs.keys.status.${item.status}`)}
                                    </Badge>
                                  </TableCell>
                                  <TableCell className='text-sm text-muted-foreground'>{formatDateTime(item.lastCheckedAt)}</TableCell>
                                  <TableCell>
                                    <div className='text-sm'>
                                      {formatBalance(item.balance, item.currency)}
                                      {item.available != null ? (
                                        <Badge variant='outline' className='ml-2'>
                                          {t(`channels.dialogs.keys.availability.${item.available ? 'available' : 'unavailable'}`)}
                                        </Badge>
                                      ) : null}
                                    </div>
                                  </TableCell>
                                  <TableCell>
                                    <div className='flex justify-end gap-1'>
                                      {item.status === 'active' ? (
                                        <Button type='button' size='sm' variant='ghost' onClick={() => handleDisableKey(item.id)} disabled={isPending}>
                                          <IconKeyOff className='h-4 w-4' />
                                        </Button>
                                      ) : null}
                                      {item.status === 'disabled' ? (
                                        <Button type='button' size='sm' variant='ghost' onClick={() => handleEnableKey(item.id)} disabled={isPending}>
                                          <IconRefresh className='h-4 w-4' />
                                        </Button>
                                      ) : null}
                                      <Popover open={confirmDeleteKey === item.id} onOpenChange={(state) => setConfirmDeleteKey(state ? item.id : null)}>
                                        <PopoverTrigger asChild>
                                          <Button type='button' size='sm' variant='ghost' className='text-destructive' disabled={isPending}>
                                            <IconTrash className='h-4 w-4' />
                                          </Button>
                                        </PopoverTrigger>
                                        <PopoverContent className='w-72'>
                                          <div className='space-y-3'>
                                            <p className='text-sm'>{t('channels.dialogs.keys.confirmDelete')}</p>
                                            <div className='flex justify-end gap-2'>
                                              <Button type='button' size='sm' variant='outline' onClick={() => setConfirmDeleteKey(null)}>
                                                {t('common.buttons.cancel')}
                                              </Button>
                                              <Button type='button' size='sm' variant='destructive' onClick={() => handleDeleteKey(item.id)} disabled={isPending}>
                                                {t('common.buttons.confirm')}
                                              </Button>
                                            </div>
                                          </div>
                                        </PopoverContent>
                                      </Popover>
                                      {item.status === 'archived' ? (
                                        <Button type='button' size='sm' variant='ghost' onClick={() => handleRestoreKey(item.id)} disabled={isPending}>
                                          <IconRestore className='h-4 w-4' />
                                        </Button>
                                      ) : (
                                        <Button type='button' size='sm' variant='ghost' onClick={() => handleArchiveKey(item.id)} disabled={isPending}>
                                          <IconArchive className='h-4 w-4' />
                                        </Button>
                                      )}
                                    </div>
                                  </TableCell>
                                </TableRow>
                              );
                            })
                          )}
                        </TableBody>
                      </Table>
                    </div>
                  </CardContent>
                </Card>
              </TabsContent>

              <TabsContent value='routing' className='mt-0 space-y-4'>
                <Card>
                  <CardHeader>
                    <CardTitle className='flex items-center gap-2'>
                      <IconRoute className='h-5 w-5' />
                      {t('channels.dialogs.keys.routing.title')}
                    </CardTitle>
                    <CardDescription>{t('channels.dialogs.keys.routing.description')}</CardDescription>
                  </CardHeader>
                  <CardContent className='space-y-4'>
                    <FormField
                      control={form.control}
                      name='strategy'
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keyRouting.fields.strategy.label')}</FormLabel>
                          <Select value={field.value} onValueChange={field.onChange}>
                            <FormControl>
                              <SelectTrigger>
                                <SelectValue placeholder={t('channels.dialogs.keyRouting.fields.strategy.placeholder')} />
                              </SelectTrigger>
                            </FormControl>
                            <SelectContent>
                              {STRATEGIES.map((strategy) => (
                                <SelectItem key={strategy} value={strategy}>
                                  {t(`channels.dialogs.keyRouting.strategies.${strategy}.label`)}
                                </SelectItem>
                              ))}
                            </SelectContent>
                          </Select>
                          <FormDescription>{t('channels.dialogs.keyRouting.fields.strategy.description')}</FormDescription>
                          <FormMessage />
                        </FormItem>
                      )}
                    />

                    <Alert>
                      <IconDatabase className='h-4 w-4' />
                      <AlertDescription>
                        <div className='font-medium'>{t(`channels.dialogs.keyRouting.strategies.${selectedStrategy}.label`)}</div>
                        <div className='mt-1 text-sm'>{t(`channels.dialogs.keyRouting.strategies.${selectedStrategy}.description`)}</div>
                      </AlertDescription>
                    </Alert>
                  </CardContent>
                </Card>
              </TabsContent>

              <TabsContent value='health' className='mt-0 space-y-4'>
                <Card>
                  <CardHeader>
                    <CardTitle className='flex items-center gap-2'>
                      <IconSettingsAutomation className='h-5 w-5' />
                      {t('channels.dialogs.keys.health.title')}
                    </CardTitle>
                    <CardDescription>{t('channels.dialogs.keys.health.description')}</CardDescription>
                  </CardHeader>
                  <CardContent className='space-y-5'>
                    <div className='grid gap-4 md:grid-cols-2'>
                      <FormField
                        control={form.control}
                        name='healthCheck.enabled'
                        render={({ field }) => (
                          <FormItem className='flex flex-row items-center justify-between rounded-lg border p-3'>
                            <div className='space-y-0.5'>
                              <FormLabel>{t('channels.dialogs.keys.health.enabled.label')}</FormLabel>
                              <FormDescription>{t('channels.dialogs.keys.health.enabled.description')}</FormDescription>
                            </div>
                            <FormControl>
                              <Switch checked={field.value} onCheckedChange={field.onChange} />
                            </FormControl>
                          </FormItem>
                        )}
                      />
                      <FormField
                        control={form.control}
                        name='healthCheck.includeDisabled'
                        render={({ field }) => (
                          <FormItem className='flex flex-row items-center justify-between rounded-lg border p-3'>
                            <div className='space-y-0.5'>
                              <FormLabel>{t('channels.dialogs.keys.health.includeDisabled.label')}</FormLabel>
                              <FormDescription>{t('channels.dialogs.keys.health.includeDisabled.description')}</FormDescription>
                            </div>
                            <FormControl>
                              <Switch checked={field.value} onCheckedChange={field.onChange} />
                            </FormControl>
                          </FormItem>
                        )}
                      />
                    </div>

                    <div className='grid gap-4 md:grid-cols-3'>
                      <FormField
                        control={form.control}
                        name='healthCheck.intervalMinutes'
                        render={({ field }) => (
                          <FormItem>
                            <FormLabel>{t('channels.dialogs.keys.health.interval.label')}</FormLabel>
                            <FormControl>
                              <Input
                                ref={field.ref}
                                name={field.name}
                                type='number'
                                min={5}
                                max={10080}
                                value={typeof field.value === 'number' || typeof field.value === 'string' ? field.value : ''}
                                onBlur={field.onBlur}
                                onChange={(event) => field.onChange(event.target.value)}
                              />
                            </FormControl>
                            <FormDescription>{t('channels.dialogs.keys.health.interval.description')}</FormDescription>
                            <FormMessage />
                          </FormItem>
                        )}
                      />
                      <FormField
                        control={form.control}
                        name='healthCheck.failureThreshold'
                        render={({ field }) => (
                          <FormItem>
                            <FormLabel>{t('channels.dialogs.keys.health.failureThreshold.label')}</FormLabel>
                            <FormControl>
                              <Input
                                ref={field.ref}
                                name={field.name}
                                type='number'
                                min={1}
                                max={20}
                                value={typeof field.value === 'number' || typeof field.value === 'string' ? field.value : ''}
                                onBlur={field.onBlur}
                                onChange={(event) => field.onChange(event.target.value)}
                              />
                            </FormControl>
                            <FormDescription>{t('channels.dialogs.keys.health.failureThreshold.description')}</FormDescription>
                            <FormMessage />
                          </FormItem>
                        )}
                      />
                      <FormField
                        control={form.control}
                        name='healthCheck.failureAction'
                        render={({ field }) => (
                          <FormItem>
                            <FormLabel>{t('channels.dialogs.keys.health.failureAction.label')}</FormLabel>
                            <Select value={field.value} onValueChange={field.onChange}>
                              <FormControl>
                                <SelectTrigger>
                                  <SelectValue />
                                </SelectTrigger>
                              </FormControl>
                              <SelectContent>
                                {FAILURE_ACTIONS.map((action) => (
                                  <SelectItem key={action} value={action}>
                                    {t(`channels.dialogs.keys.health.failureActions.${action}`)}
                                  </SelectItem>
                                ))}
                              </SelectContent>
                            </Select>
                            <FormDescription>{t('channels.dialogs.keys.health.failureAction.description')}</FormDescription>
                          </FormItem>
                        )}
                      />
                    </div>

                    <Separator />

                    <div className='space-y-4'>
                      <FormField
                        control={form.control}
                        name='healthCheck.builtinRuleEnabled'
                        render={({ field }) => (
                          <FormItem className='flex flex-row items-center justify-between rounded-lg border p-3'>
                            <div className='space-y-0.5'>
                              <FormLabel>{t('channels.dialogs.keys.health.rules.builtin.label')}</FormLabel>
                              <FormDescription>{t('channels.dialogs.keys.health.rules.builtin.description')}</FormDescription>
                            </div>
                            <FormControl>
                              <Switch checked={field.value} onCheckedChange={field.onChange} />
                            </FormControl>
                          </FormItem>
                        )}
                      />

                      <FormField
                        control={form.control}
                        name='healthCheck.deepseekRuleEnabled'
                        render={({ field }) => (
                          <FormItem className='flex flex-row items-center justify-between rounded-lg border p-3'>
                            <div className='space-y-0.5'>
                              <FormLabel>{t('channels.dialogs.keys.health.rules.deepseek.label')}</FormLabel>
                              <FormDescription>{t('channels.dialogs.keys.health.rules.deepseek.description')}</FormDescription>
                            </div>
                            <FormControl>
                              <Switch checked={field.value} onCheckedChange={field.onChange} />
                            </FormControl>
                          </FormItem>
                        )}
                      />

                      {deepseekRuleEnabled ? (
                        <div className='grid gap-4 rounded-lg border p-3 md:grid-cols-3'>
                          <FormField
                            control={form.control}
                            name='healthCheck.deepseekPath'
                            render={({ field }) => (
                              <FormItem>
                                <FormLabel>{t('channels.dialogs.keys.health.rules.deepseek.path')}</FormLabel>
                                <FormControl>
                                  <Input {...field} />
                                </FormControl>
                                <FormMessage />
                              </FormItem>
                            )}
                          />
                          <FormField
                            control={form.control}
                            name='healthCheck.deepseekExpectedStatuses'
                            render={({ field }) => (
                              <FormItem>
                                <FormLabel>{t('channels.dialogs.keys.health.rules.deepseek.statuses')}</FormLabel>
                                <FormControl>
                                  <Input {...field} />
                                </FormControl>
                              </FormItem>
                            )}
                          />
                          <FormField
                            control={form.control}
                            name='healthCheck.deepseekPassWhen'
                            render={({ field }) => (
                              <FormItem className='md:col-span-3'>
                                <FormLabel>{t('channels.dialogs.keys.health.rules.deepseek.passWhen')}</FormLabel>
                                <FormControl>
                                  <Textarea rows={3} {...field} />
                                </FormControl>
                                <FormDescription>{t('channels.dialogs.keys.health.rules.deepseek.passWhenDescription')}</FormDescription>
                              </FormItem>
                            )}
                          />
                        </div>
                      ) : null}
                    </div>
                  </CardContent>
                </Card>
              </TabsContent>
            </ScrollArea>
          </Tabs>
        </Form>

        <DialogFooter className='gap-2 sm:justify-between'>
          <div className='flex items-center gap-2'>
            <Button type='button' variant='outline' onClick={handleRunChecks} disabled={isPending || activeKeys.length === 0}>
              {testAPIKeys.isPending ? <IconLoader2 className='mr-2 h-4 w-4 animate-spin' /> : <IconPlayerPlay className='mr-2 h-4 w-4' />}
              {t('channels.dialogs.keys.actions.runChecks')}
            </Button>
            <div className='hidden items-center gap-1 text-xs text-muted-foreground sm:flex'>
              <IconAlertTriangle className='h-3.5 w-3.5' />
              {t('channels.dialogs.keys.rawKeySafety')}
            </div>
          </div>
          <div className='flex gap-2'>
            <Button type='button' variant='outline' onClick={() => onOpenChange(false)}>
              {t('common.buttons.cancel')}
            </Button>
            <Button type='button' onClick={form.handleSubmit(handleSaveSettings)} disabled={isPending || !form.formState.isValid}>
              {updateChannel.isPending ? t('common.buttons.saving') : t('common.buttons.save')}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
