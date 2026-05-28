import { useEffect, useMemo, useState } from 'react';
import { IconCircleCheck, IconCircleX, IconPlus, IconRefresh, IconTrash } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { extractNumberID, extractNumberIDAsNumber } from '@/lib/utils';
import { usePermissions } from '@/hooks/usePermissions';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Checkbox } from '@/components/ui/checkbox';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Separator } from '@/components/ui/separator';
import { Switch } from '@/components/ui/switch';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { Textarea } from '@/components/ui/textarea';
import { Header } from '@/components/layout/header';
import { Main } from '@/components/layout/main';
import { useAllChannelSummarys } from '@/features/channels/data/channels';
import type {
  ChannelKeyHealthCheckRule,
  ChannelKeyStatus,
  FailurePolicyActionType,
  FailurePolicyConditionCombiner,
  FailurePolicyEventSource,
  FailurePolicyProfile,
} from '@/features/channels/data/schema';
import {
  useMonitoringEvents,
  useMonitoringSettings,
  useUpdateMonitoringSettings,
  type MonitoringRule,
  type MonitoringSettings,
} from './data/monitoring';

type ProfileTarget = 'key' | 'channel';
type OutcomeFilter = 'all' | 'success' | 'failure' | 'skipped';

const KEY_STATUSES: ChannelKeyStatus[] = ['active', 'disabled', 'archived'];
const CHANNEL_STATUSES = ['enabled', 'disabled', 'archived'];
const PROFILE_SOURCE_SCENES: Array<{ id: string; sources: FailurePolicyEventSource[] }> = [
  { id: 'scheduledHealth', sources: ['scheduled_health_check', 'scheduled_health_check_failure'] },
  { id: 'manualHealth', sources: ['manual_health_check', 'manual_health_check_failure'] },
  { id: 'scheduledBalance', sources: ['scheduled_balance_probe', 'scheduled_balance_probe_failure'] },
  { id: 'manualBalance', sources: ['manual_balance_probe', 'manual_balance_probe_failure'] },
];
const CONDITION_COMBINERS: FailurePolicyConditionCombiner[] = ['or', 'and'];
const KEY_ACTIONS: FailurePolicyActionType[] = [
  'report_only',
  'backoff_key',
  'disable_key',
  'archive_key',
  'delete_key',
  'enable_key',
  'restore_key',
];
const CHANNEL_ACTIONS: FailurePolicyActionType[] = ['report_only', 'disable_channel'];

function createBuiltinProbe(index = 0) {
  return {
    id: `probe-${Date.now()}-${index + 1}`,
    name: 'Builtin key check',
    type: 'builtin_test' as const,
    enabled: true,
    builtin: {
      kind: 'channel_api_key_test' as const,
    },
    http: null,
  };
}

function createChannelBalanceProbe(index = 0): ChannelKeyHealthCheckRule {
  return {
    id: `channel-balance-probe-${Date.now()}-${index + 1}`,
    name: 'Channel balance probe',
    type: 'channel_balance_probe',
    enabled: true,
    builtin: null,
    http: null,
  };
}

function createProfile(target: ProfileTarget, kind: 'failure' | 'recovery', index = 0): FailurePolicyProfile {
  const isRecovery = kind === 'recovery';

  return {
    id: `${target}-${kind}-${Date.now()}-${index + 1}`,
    name: isRecovery
      ? target === 'key'
        ? 'Recover healthy key'
        : 'Recover channel'
      : target === 'key'
        ? 'Disable failed key'
        : 'Disable failed channel',
    enabled: true,
    sources: isRecovery
      ? ['scheduled_balance_probe', 'manual_balance_probe']
      : ['scheduled_balance_probe_failure', 'manual_balance_probe_failure'],
    conditionCombiner: 'or',
    conditions: {
      minFailureCount: isRecovery ? null : 3,
      success: null,
      statusCodes: [],
      available: isRecovery ? true : null,
      balanceLTE: null,
      balanceGTE: isRecovery ? 0 : null,
      reasonContains: null,
      keyStatuses: isRecovery && target === 'key' ? ['disabled', 'archived'] : null,
      expr: null,
    },
    actions:
      target === 'key'
        ? [
            {
              type: isRecovery ? 'restore_key' : 'disable_key',
              backoff: null,
            },
            ...(isRecovery
              ? [
                  {
                    type: 'enable_key' as const,
                    backoff: null,
                  },
                ]
              : []),
          ]
        : [
            {
              type: isRecovery ? 'report_only' : 'disable_channel',
              backoff: null,
            },
          ],
  };
}

function createRule(index = 0): MonitoringRule {
  return {
    id: `monitor-rule-${Date.now()}-${index + 1}`,
    name: `Monitoring rule ${index + 1}`,
    description: '',
    enabled: true,
    probeType: 'channel_balance_probe',
    schedule: {
      intervalMinutes: 60,
      historyLimit: 100,
      maxChannels: 0,
      maxKeysPerChannel: 0,
      keySpacingMs: 1000,
      jitterMs: 250,
    },
    targets: {
      channelIDs: [],
      channelStatuses: ['enabled'],
      keyStatuses: ['active'],
      includeBackoff: false,
    },
    probes: [createChannelBalanceProbe(index)],
    keyProfiles: [createProfile('key', 'failure', index), createProfile('key', 'recovery', index)],
    channelProfiles: [],
  };
}

function createDefaultSettings(): MonitoringSettings {
  return {
    enabled: false,
    historyRetentionDays: 30,
    rules: [],
  };
}

function formatDate(value?: string | null) {
  if (!value) return '-';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString();
}

function numericValue(value: string): number | null {
  if (value.trim() === '') return null;
  const next = Number(value);
  return Number.isFinite(next) ? next : null;
}

function profileSourceSceneState(sources: FailurePolicyEventSource[] | undefined | null, sceneSources: FailurePolicyEventSource[]) {
  const selectedCount = sceneSources.filter((source) => (sources ?? []).includes(source)).length;
  if (selectedCount === 0) return false;
  return selectedCount === sceneSources.length ? true : 'indeterminate';
}

function setProfileSourceScene(
  sources: FailurePolicyEventSource[] | undefined | null,
  sceneSources: FailurePolicyEventSource[],
  checked: boolean
): FailurePolicyEventSource[] {
  const sourceSet = new Set(sources ?? []);
  if (checked) {
    sceneSources.forEach((source) => sourceSet.add(source));
  } else {
    sceneSources.forEach((source) => sourceSet.delete(source));
  }
  return [...sourceSet];
}

function profileForSave(profile: FailurePolicyProfile): FailurePolicyProfile {
  const { allCheckedKeysFailed: _allCheckedKeysFailed, ...conditions } = profile.conditions ?? {};
  return {
    ...profile,
    conditionCombiner: profile.conditionCombiner ?? 'or',
    conditions,
  };
}

function formatActionLabel(action: string) {
  return action.replaceAll('_', ' ');
}

function MonitoringManagement() {
  const { t } = useTranslation();
  const { hasScope } = usePermissions();
  const canWrite = hasScope('write_channels');
  const { data: settings, isLoading, refetch } = useMonitoringSettings();
  const updateSettings = useUpdateMonitoringSettings();
  const { data: channelsData } = useAllChannelSummarys(undefined, { includeArchived: true });

  const channels = useMemo(() => channelsData?.edges?.map((edge) => edge.node) ?? [], [channelsData]);
  const [draft, setDraft] = useState<MonitoringSettings>(createDefaultSettings);
  const [selectedRuleId, setSelectedRuleId] = useState<string | null>(null);
  const [dirty, setDirty] = useState(false);

  useEffect(() => {
    if (!settings || dirty) return;
    setDraft(settings);
    setSelectedRuleId((current) => current ?? settings.rules[0]?.id ?? null);
  }, [settings, dirty]);

  const selectedRule = draft.rules.find((rule) => rule.id === selectedRuleId) ?? draft.rules[0] ?? null;

  const markDirty = (updater: (current: MonitoringSettings) => MonitoringSettings) => {
    setDraft((current) => updater(current));
    setDirty(true);
  };

  const patchSelectedRule = (updater: (rule: MonitoringRule) => MonitoringRule) => {
    if (!selectedRule) return;
    markDirty((current) => ({
      ...current,
      rules: current.rules.map((rule) => (rule.id === selectedRule.id ? updater(rule) : rule)),
    }));
  };

  const addRule = () => {
    const nextRule = createRule(draft.rules.length);
    markDirty((current) => ({
      ...current,
      rules: [...current.rules, nextRule],
    }));
    setSelectedRuleId(nextRule.id);
  };

  const deleteSelectedRule = () => {
    if (!selectedRule) return;
    const nextRules = draft.rules.filter((rule) => rule.id !== selectedRule.id);
    markDirty((current) => ({
      ...current,
      rules: nextRules,
    }));
    setSelectedRuleId(nextRules[0]?.id ?? null);
  };

  const resetDraft = () => {
    const next = settings ?? createDefaultSettings();
    setDraft(next);
    setSelectedRuleId(next.rules[0]?.id ?? null);
    setDirty(false);
  };

  const save = async () => {
    const invalidRule = draft.rules.find((rule) => {
      return (
        rule.name.trim().length === 0 ||
        rule.schedule.intervalMinutes < 1 ||
        rule.targets.keyStatuses.length === 0 ||
        rule.probes.filter((probe) => probe.enabled !== false).length === 0
      );
    });

    if (invalidRule) {
      toast.error(t('monitoring.validation.ruleInvalid', { name: invalidRule.name || invalidRule.id }));
      return;
    }

    await updateSettings.mutateAsync({
      enabled: draft.enabled,
      historyRetentionDays: draft.historyRetentionDays,
      rules: draft.rules.map((rule) => ({
        ...rule,
        name: rule.name.trim(),
        description: rule.description?.trim() || null,
        keyProfiles: rule.keyProfiles.map(profileForSave),
        channelProfiles: rule.channelProfiles.map(profileForSave),
      })),
    });
    setDirty(false);
  };

  return (
    <>
      <Header fixed>
        <div className='flex flex-1 items-center justify-between gap-4'>
          <div>
            <h2 className='text-xl font-bold tracking-tight'>{t('monitoring.title')}</h2>
            <p className='text-muted-foreground text-sm'>{t('monitoring.description')}</p>
          </div>
          <div className='flex items-center gap-2'>
            <Button variant='outline' onClick={() => refetch()} disabled={isLoading}>
              <IconRefresh className='mr-2 h-4 w-4' />
              {t('common.refresh')}
            </Button>
            <Button onClick={save} disabled={!canWrite || updateSettings.isPending || isLoading}>
              {updateSettings.isPending ? t('common.buttons.saving') : t('common.buttons.saveChanges')}
            </Button>
          </div>
        </div>
      </Header>

      <Main>
        <div className='flex flex-col gap-4'>
          <Card>
            <CardHeader>
              <CardTitle>{t('monitoring.global.title')}</CardTitle>
              <CardDescription>{t('monitoring.global.description')}</CardDescription>
            </CardHeader>
            <CardContent className='grid gap-4 md:grid-cols-[minmax(0,1fr)_220px]'>
              <div className='flex items-center justify-between rounded-lg border p-4'>
                <div>
                  <Label>{t('monitoring.global.enabled')}</Label>
                  <p className='text-muted-foreground text-sm'>{t('monitoring.global.enabledHint')}</p>
                </div>
                <Switch
                  checked={draft.enabled}
                  disabled={!canWrite}
                  onCheckedChange={(checked) => markDirty((current) => ({ ...current, enabled: checked }))}
                />
              </div>
              <div className='space-y-2'>
                <Label htmlFor='monitoring-history-retention'>{t('monitoring.global.retentionDays')}</Label>
                <Input
                  id='monitoring-history-retention'
                  type='number'
                  min={1}
                  value={draft.historyRetentionDays}
                  disabled={!canWrite}
                  onChange={(event) => {
                    const next = Number(event.target.value);
                    if (!Number.isFinite(next)) return;
                    markDirty((current) => ({ ...current, historyRetentionDays: Math.max(1, Math.floor(next)) }));
                  }}
                />
              </div>
            </CardContent>
          </Card>

          <div className='grid min-h-[580px] gap-4 lg:grid-cols-[320px_minmax(0,1fr)]'>
            <Card className='overflow-hidden'>
              <CardHeader className='flex flex-row items-start justify-between gap-2 space-y-0'>
                <div>
                  <CardTitle>{t('monitoring.rules.title')}</CardTitle>
                  <CardDescription>{t('monitoring.rules.description')}</CardDescription>
                </div>
                <Button size='sm' onClick={addRule} disabled={!canWrite}>
                  <IconPlus className='mr-1 h-4 w-4' />
                  {t('monitoring.rules.add')}
                </Button>
              </CardHeader>
              <CardContent className='space-y-2'>
                {draft.rules.length === 0 ? (
                  <div className='text-muted-foreground rounded-lg border border-dashed p-4 text-sm'>{t('monitoring.rules.empty')}</div>
                ) : (
                  draft.rules.map((rule) => (
                    <button
                      key={rule.id}
                      type='button'
                      onClick={() => setSelectedRuleId(rule.id)}
                      className={`w-full rounded-lg border p-3 text-left transition-colors ${
                        selectedRule?.id === rule.id ? 'border-primary bg-primary/5' : 'hover:bg-muted/60'
                      }`}
                    >
                      <div className='flex items-start justify-between gap-2'>
                        <div className='min-w-0'>
                          <div className='truncate font-medium'>{rule.name}</div>
                          <div className='text-muted-foreground mt-1 text-xs'>
                            {t('monitoring.rules.intervalSummary', { minutes: rule.schedule.intervalMinutes })}
                          </div>
                        </div>
                        <Badge variant={rule.enabled === false ? 'secondary' : 'default'}>
                          {rule.enabled === false ? t('monitoring.status.disabled') : t('monitoring.status.enabled')}
                        </Badge>
                      </div>
                      <div className='mt-2 flex flex-wrap gap-1'>
                        {rule.targets.keyStatuses.map((status) => (
                          <Badge key={status} variant='outline'>
                            {t(`monitoring.keyStatuses.${status}`)}
                          </Badge>
                        ))}
                      </div>
                    </button>
                  ))
                )}
              </CardContent>
            </Card>

            <Card className='overflow-hidden'>
              <CardHeader className='flex flex-row items-start justify-between gap-2 space-y-0'>
                <div>
                  <CardTitle>{selectedRule ? t('monitoring.editor.title') : t('monitoring.editor.noRuleTitle')}</CardTitle>
                  <CardDescription>
                    {selectedRule ? t('monitoring.editor.description') : t('monitoring.editor.noRuleDescription')}
                  </CardDescription>
                </div>
                {selectedRule ? (
                  <Button variant='destructive' size='sm' onClick={deleteSelectedRule} disabled={!canWrite}>
                    <IconTrash className='mr-1 h-4 w-4' />
                    {t('common.buttons.delete')}
                  </Button>
                ) : null}
              </CardHeader>
              <CardContent>
                {selectedRule ? (
                  <Tabs defaultValue='basics' className='space-y-4'>
                    <TabsList className='grid h-auto w-full grid-cols-2 gap-1 md:grid-cols-5'>
                      <TabsTrigger value='basics'>{t('monitoring.editor.basics')}</TabsTrigger>
                      <TabsTrigger value='targets'>{t('monitoring.targets.title')}</TabsTrigger>
                      <TabsTrigger value='probes'>{t('monitoring.probes.title')}</TabsTrigger>
                      <TabsTrigger value='keyProfiles'>{t('monitoring.profiles.keyTitle')}</TabsTrigger>
                      <TabsTrigger value='channelProfiles'>{t('monitoring.profiles.channelTitle')}</TabsTrigger>
                    </TabsList>
                    <TabsContent value='basics' className='mt-0'>
                      <RuleBasics rule={selectedRule} canWrite={canWrite} onChange={patchSelectedRule} />
                    </TabsContent>
                    <TabsContent value='targets' className='mt-0'>
                      <RuleTargets rule={selectedRule} channels={channels} canWrite={canWrite} onChange={patchSelectedRule} />
                    </TabsContent>
                    <TabsContent value='probes' className='mt-0'>
                      <RuleProbes rule={selectedRule} canWrite={canWrite} onChange={patchSelectedRule} />
                    </TabsContent>
                    <TabsContent value='keyProfiles' className='mt-0'>
                      <ProfilesEditor
                        title={t('monitoring.profiles.keyTitle')}
                        target='key'
                        rule={selectedRule}
                        canWrite={canWrite}
                        onChange={patchSelectedRule}
                      />
                    </TabsContent>
                    <TabsContent value='channelProfiles' className='mt-0'>
                      <ProfilesEditor
                        title={t('monitoring.profiles.channelTitle')}
                        target='channel'
                        rule={selectedRule}
                        canWrite={canWrite}
                        onChange={patchSelectedRule}
                      />
                    </TabsContent>
                  </Tabs>
                ) : (
                  <div className='text-muted-foreground rounded-lg border border-dashed p-8 text-center'>
                    {t('monitoring.editor.empty')}
                  </div>
                )}
              </CardContent>
            </Card>
          </div>

          <MonitoringHistory rules={draft.rules} channels={channels} />

          {dirty ? (
            <div className='bg-background sticky bottom-4 ml-auto flex w-fit items-center gap-2 rounded-lg border p-2 shadow-lg'>
              <span className='text-muted-foreground px-2 text-sm'>{t('common.unsavedChanges')}</span>
              <Button variant='outline' size='sm' onClick={resetDraft}>
                {t('common.buttons.cancel')}
              </Button>
              <Button size='sm' onClick={save} disabled={!canWrite || updateSettings.isPending}>
                {updateSettings.isPending ? t('common.buttons.saving') : t('common.buttons.save')}
              </Button>
            </div>
          ) : null}
        </div>
      </Main>
    </>
  );
}

function RuleBasics({
  rule,
  canWrite,
  onChange,
}: {
  rule: MonitoringRule;
  canWrite: boolean;
  onChange: (updater: (rule: MonitoringRule) => MonitoringRule) => void;
}) {
  const { t } = useTranslation();

  return (
    <section className='space-y-4'>
      <div className='flex items-center justify-between gap-4'>
        <div>
          <h3 className='font-semibold'>{t('monitoring.editor.basics')}</h3>
          <p className='text-muted-foreground text-sm'>{t('monitoring.editor.basicsHint')}</p>
        </div>
        <Switch
          checked={rule.enabled !== false}
          disabled={!canWrite}
          onCheckedChange={(checked) => onChange((current) => ({ ...current, enabled: checked }))}
        />
      </div>
      <div className='grid gap-4 md:grid-cols-2'>
        <div className='space-y-2'>
          <Label>{t('common.columns.name')}</Label>
          <Input
            value={rule.name}
            disabled={!canWrite}
            onChange={(event) => onChange((current) => ({ ...current, name: event.target.value }))}
          />
        </div>
        <div className='space-y-2'>
          <Label>{t('monitoring.schedule.intervalMinutes')}</Label>
          <Input
            type='number'
            min={1}
            value={rule.schedule.intervalMinutes}
            disabled={!canWrite}
            onChange={(event) => {
              const next = Number(event.target.value);
              if (!Number.isFinite(next)) return;
              onChange((current) => ({
                ...current,
                schedule: { ...current.schedule, intervalMinutes: Math.max(1, Math.floor(next)) },
              }));
            }}
          />
        </div>
      </div>
      <div className='space-y-2'>
        <Label>{t('common.columns.description')}</Label>
        <Textarea
          value={rule.description ?? ''}
          disabled={!canWrite}
          onChange={(event) => onChange((current) => ({ ...current, description: event.target.value }))}
        />
      </div>
      <div className='grid gap-4 md:grid-cols-4'>
        <NumberField
          label={t('monitoring.schedule.maxChannels')}
          value={rule.schedule.maxChannels}
          disabled={!canWrite}
          onChange={(value) => onChange((current) => ({ ...current, schedule: { ...current.schedule, maxChannels: value } }))}
        />
        <NumberField
          label={t('monitoring.schedule.maxKeysPerChannel')}
          value={rule.schedule.maxKeysPerChannel}
          disabled={!canWrite}
          onChange={(value) => onChange((current) => ({ ...current, schedule: { ...current.schedule, maxKeysPerChannel: value } }))}
        />
        <NumberField
          label={t('monitoring.schedule.keySpacingMs')}
          value={rule.schedule.keySpacingMs}
          disabled={!canWrite}
          onChange={(value) => onChange((current) => ({ ...current, schedule: { ...current.schedule, keySpacingMs: value } }))}
        />
        <NumberField
          label={t('monitoring.schedule.jitterMs')}
          value={rule.schedule.jitterMs}
          disabled={!canWrite}
          onChange={(value) => onChange((current) => ({ ...current, schedule: { ...current.schedule, jitterMs: value } }))}
        />
      </div>
    </section>
  );
}

function NumberField({
  label,
  value,
  disabled,
  onChange,
}: {
  label: string;
  value: number;
  disabled?: boolean;
  onChange: (value: number) => void;
}) {
  return (
    <div className='space-y-2'>
      <Label>{label}</Label>
      <Input
        type='number'
        min={0}
        value={value}
        disabled={disabled}
        onChange={(event) => {
          const next = Number(event.target.value);
          if (Number.isFinite(next)) onChange(Math.max(0, Math.floor(next)));
        }}
      />
    </div>
  );
}

function RuleTargets({
  rule,
  channels,
  canWrite,
  onChange,
}: {
  rule: MonitoringRule;
  channels: Array<{ id: string; name: string; status: string }>;
  canWrite: boolean;
  onChange: (updater: (rule: MonitoringRule) => MonitoringRule) => void;
}) {
  const { t } = useTranslation();
  const selectedChannelIDs = new Set(rule.targets.channelIDs);

  const toggleTarget = <T,>(values: T[], value: T, checked: boolean) =>
    checked ? [...values, value] : values.filter((item) => item !== value);

  return (
    <section className='space-y-4'>
      <div>
        <h3 className='font-semibold'>{t('monitoring.targets.title')}</h3>
        <p className='text-muted-foreground text-sm'>{t('monitoring.targets.description')}</p>
      </div>
      <div className='grid gap-4 lg:grid-cols-3'>
        <div className='space-y-3 rounded-lg border p-3'>
          <div className='flex items-center justify-between'>
            <Label>{t('monitoring.targets.channels')}</Label>
            <Button
              type='button'
              variant='ghost'
              size='sm'
              disabled={!canWrite}
              onClick={() => onChange((current) => ({ ...current, targets: { ...current.targets, channelIDs: [] } }))}
            >
              {t('monitoring.targets.allChannels')}
            </Button>
          </div>
          <div className='max-h-52 space-y-2 overflow-auto pr-1'>
            {channels.map((channel) => {
              const numericID = extractNumberIDAsNumber(channel.id);
              return (
                <label key={channel.id} className='flex cursor-pointer items-center gap-2 text-sm'>
                  <Checkbox
                    checked={selectedChannelIDs.has(numericID)}
                    disabled={!canWrite}
                    onCheckedChange={(checked) =>
                      onChange((current) => ({
                        ...current,
                        targets: {
                          ...current.targets,
                          channelIDs: toggleTarget(current.targets.channelIDs, numericID, Boolean(checked)),
                        },
                      }))
                    }
                  />
                  <span className='min-w-0 flex-1 truncate'>
                    #{extractNumberID(channel.id)} {channel.name}
                  </span>
                  <Badge variant='outline'>{channel.status}</Badge>
                </label>
              );
            })}
            {channels.length === 0 ? <div className='text-muted-foreground text-sm'>{t('monitoring.targets.noChannels')}</div> : null}
          </div>
          {rule.targets.channelIDs.length === 0 ? (
            <p className='text-muted-foreground text-xs'>{t('monitoring.targets.allChannelsHint')}</p>
          ) : null}
        </div>

        <div className='space-y-3 rounded-lg border p-3'>
          <Label>{t('monitoring.targets.channelStatuses')}</Label>
          {CHANNEL_STATUSES.map((status) => (
            <label key={status} className='flex cursor-pointer items-center gap-2 text-sm'>
              <Checkbox
                checked={rule.targets.channelStatuses.includes(status)}
                disabled={!canWrite}
                onCheckedChange={(checked) =>
                  onChange((current) => ({
                    ...current,
                    targets: {
                      ...current.targets,
                      channelStatuses: toggleTarget(current.targets.channelStatuses, status, Boolean(checked)),
                    },
                  }))
                }
              />
              {t(`monitoring.channelStatuses.${status}`)}
            </label>
          ))}
        </div>

        <div className='space-y-3 rounded-lg border p-3'>
          <Label>{t('monitoring.targets.keyStatuses')}</Label>
          {KEY_STATUSES.map((status) => (
            <label key={status} className='flex cursor-pointer items-center gap-2 text-sm'>
              <Checkbox
                checked={rule.targets.keyStatuses.includes(status)}
                disabled={!canWrite}
                onCheckedChange={(checked) =>
                  onChange((current) => ({
                    ...current,
                    targets: {
                      ...current.targets,
                      keyStatuses: toggleTarget(current.targets.keyStatuses, status, Boolean(checked)),
                    },
                  }))
                }
              />
              {t(`monitoring.keyStatuses.${status}`)}
            </label>
          ))}
          <Separator />
          <label className='flex cursor-pointer items-center justify-between gap-2 text-sm'>
            <span>{t('monitoring.targets.includeBackoff')}</span>
            <Switch
              checked={rule.targets.includeBackoff}
              disabled={!canWrite}
              onCheckedChange={(checked) =>
                onChange((current) => ({ ...current, targets: { ...current.targets, includeBackoff: checked } }))
              }
            />
          </label>
        </div>
      </div>
    </section>
  );
}

function RuleProbes({
  rule,
  canWrite,
  onChange,
}: {
  rule: MonitoringRule;
  canWrite: boolean;
  onChange: (updater: (rule: MonitoringRule) => MonitoringRule) => void;
}) {
  const { t } = useTranslation();
  const updateProbe = (id: string, patch: Partial<ChannelKeyHealthCheckRule>) => {
    onChange((current) => ({
      ...current,
      probes: current.probes.map((item) => (item.id === id ? { ...item, ...patch } : item)),
    }));
  };

  return (
    <section className='space-y-4'>
      <div className='flex items-center justify-between gap-2'>
        <div>
          <h3 className='font-semibold'>{t('monitoring.probes.title')}</h3>
          <p className='text-muted-foreground text-sm'>{t('monitoring.probes.description')}</p>
        </div>
        <div className='flex flex-wrap gap-2'>
          <Button
            type='button'
            variant='outline'
            size='sm'
            disabled={!canWrite}
            onClick={() => onChange((current) => ({ ...current, probes: [...current.probes, createBuiltinProbe(current.probes.length)] }))}
          >
            <IconPlus className='mr-1 h-4 w-4' />
            {t('monitoring.probes.addBuiltin')}
          </Button>
          <Button
            type='button'
            variant='outline'
            size='sm'
            disabled={!canWrite}
            onClick={() =>
              onChange((current) => ({ ...current, probes: [...current.probes, createChannelBalanceProbe(current.probes.length)] }))
            }
          >
            <IconPlus className='mr-1 h-4 w-4' />
            {t('monitoring.probes.addChannelBalance')}
          </Button>
        </div>
      </div>
      <div className='space-y-2'>
        {rule.probes.map((probe) => (
          <div key={probe.id} className='space-y-3 rounded-lg border p-3'>
            <div className='grid gap-2 md:grid-cols-[auto_minmax(0,1fr)_180px_auto] md:items-center'>
              <Switch
                checked={probe.enabled !== false}
                disabled={!canWrite}
                onCheckedChange={(checked) => updateProbe(probe.id, { enabled: checked })}
              />
              <Input value={probe.name} disabled={!canWrite} onChange={(event) => updateProbe(probe.id, { name: event.target.value })} />
              <Select
                value={probe.type}
                disabled={!canWrite}
                onValueChange={(value) =>
                  updateProbe(
                    probe.id,
                    value === 'builtin_test'
                      ? {
                          type: 'builtin_test',
                          builtin: { kind: 'channel_api_key_test' },
                          http: null,
                        }
                      : {
                          ...createChannelBalanceProbe(0),
                          id: probe.id,
                          name: probe.name,
                          enabled: probe.enabled,
                        }
                  )
                }
              >
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value='builtin_test'>{t('monitoring.probes.types.builtin')}</SelectItem>
                  <SelectItem value='channel_balance_probe'>{t('monitoring.probes.types.channelBalance')}</SelectItem>
                  {probe.type === 'http' ? <SelectItem value='http'>{t('monitoring.probes.types.legacyHttp')}</SelectItem> : null}
                </SelectContent>
              </Select>
              <div className='flex items-center justify-end gap-2'>
                <Badge variant='outline'>{probe.type}</Badge>
                <Button
                  type='button'
                  variant='ghost'
                  size='icon'
                  disabled={!canWrite || rule.probes.length <= 1}
                  onClick={() => onChange((current) => ({ ...current, probes: current.probes.filter((item) => item.id !== probe.id) }))}
                >
                  <IconTrash className='h-4 w-4' />
                </Button>
              </div>
            </div>
            {probe.type === 'channel_balance_probe' ? (
              <div className='bg-muted/30 text-muted-foreground rounded-md p-3 text-sm'>{t('monitoring.probes.channelBalance.hint')}</div>
            ) : null}
            {probe.type === 'http' ? (
              <div className='bg-muted/30 text-muted-foreground rounded-md p-3 text-sm'>{t('monitoring.probes.legacyHttp.hint')}</div>
            ) : null}
          </div>
        ))}
      </div>
    </section>
  );
}

function ProfilesEditor({
  title,
  target,
  rule,
  canWrite,
  onChange,
}: {
  title: string;
  target: ProfileTarget;
  rule: MonitoringRule;
  canWrite: boolean;
  onChange: (updater: (rule: MonitoringRule) => MonitoringRule) => void;
}) {
  const { t } = useTranslation();
  const profileKey = target === 'key' ? 'keyProfiles' : 'channelProfiles';
  const profiles = rule[profileKey];

  const updateProfile = (profileId: string, updater: (profile: FailurePolicyProfile) => FailurePolicyProfile) => {
    onChange((current) => ({
      ...current,
      [profileKey]: current[profileKey].map((profile) => (profile.id === profileId ? updater(profile) : profile)),
    }));
  };

  return (
    <section className='space-y-4'>
      <div className='flex items-center justify-between gap-2'>
        <div>
          <h3 className='font-semibold'>{title}</h3>
          <p className='text-muted-foreground text-sm'>{t('monitoring.profiles.description')}</p>
        </div>
        <div className='flex gap-2'>
          <Button
            type='button'
            variant='outline'
            size='sm'
            disabled={!canWrite}
            onClick={() =>
              onChange((current) => ({
                ...current,
                [profileKey]: [...current[profileKey], createProfile(target, 'failure', profiles.length)],
              }))
            }
          >
            {t('monitoring.profiles.addFailure')}
          </Button>
          <Button
            type='button'
            variant='outline'
            size='sm'
            disabled={!canWrite}
            onClick={() =>
              onChange((current) => ({
                ...current,
                [profileKey]: [...current[profileKey], createProfile(target, 'recovery', profiles.length)],
              }))
            }
          >
            {t('monitoring.profiles.addRecovery')}
          </Button>
        </div>
      </div>

      {profiles.length === 0 ? (
        <div className='text-muted-foreground rounded-lg border border-dashed p-4 text-sm'>{t('monitoring.profiles.empty')}</div>
      ) : null}

      {profiles.map((profile) => (
        <div key={profile.id} className='space-y-4 rounded-lg border p-4'>
          <div className='flex flex-wrap items-center justify-between gap-3'>
            <div className='flex min-w-64 flex-1 items-center gap-2'>
              <Switch
                checked={profile.enabled !== false}
                disabled={!canWrite}
                onCheckedChange={(checked) => updateProfile(profile.id, (current) => ({ ...current, enabled: checked }))}
              />
              <Input
                value={profile.name}
                disabled={!canWrite}
                onChange={(event) => updateProfile(profile.id, (current) => ({ ...current, name: event.target.value }))}
              />
            </div>
            <Button
              type='button'
              variant='ghost'
              size='icon'
              disabled={!canWrite}
              onClick={() =>
                onChange((current) => ({ ...current, [profileKey]: current[profileKey].filter((item) => item.id !== profile.id) }))
              }
            >
              <IconTrash className='h-4 w-4' />
            </Button>
          </div>

          <div className='grid gap-4 lg:grid-cols-3'>
            <div className='space-y-2'>
              <Label>{t('monitoring.profiles.triggerScenes')}</Label>
              <div className='space-y-2 rounded-md border p-3'>
                {PROFILE_SOURCE_SCENES.map((scene) => (
                  <label key={scene.id} className='flex cursor-pointer items-center gap-2 text-sm'>
                    <Checkbox
                      checked={profileSourceSceneState(profile.sources, scene.sources)}
                      disabled={!canWrite}
                      onCheckedChange={(checked) =>
                        updateProfile(profile.id, (current) => ({
                          ...current,
                          sources: (() => {
                            const nextSources = setProfileSourceScene(current.sources, scene.sources, checked === true);
                            return nextSources.length === 0 && (current.sources ?? []).length > 0 ? current.sources : nextSources;
                          })(),
                        }))
                      }
                    />
                    {t(`monitoring.triggerScenes.${scene.id}`)}
                  </label>
                ))}
              </div>
            </div>

            <div className='space-y-2'>
              <Label>{t('monitoring.profiles.conditions')}</Label>
              <div className='grid gap-2 rounded-md border p-3'>
                <Select
                  value={profile.conditionCombiner ?? 'or'}
                  disabled={!canWrite}
                  onValueChange={(value) =>
                    updateProfile(profile.id, (current) => ({
                      ...current,
                      conditionCombiner: value as FailurePolicyConditionCombiner,
                    }))
                  }
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {CONDITION_COMBINERS.map((combiner) => (
                      <SelectItem key={combiner} value={combiner}>
                        {t(`monitoring.conditionCombiners.${combiner}`)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <Select
                  value={profile.conditions?.success == null ? 'any' : profile.conditions.success ? 'success' : 'failure'}
                  disabled={!canWrite}
                  onValueChange={(value) =>
                    updateProfile(profile.id, (current) => ({
                      ...current,
                      conditions: {
                        ...(current.conditions ?? {}),
                        success: value === 'any' ? null : value === 'success',
                      },
                    }))
                  }
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value='any'>{t('monitoring.conditions.anyOutcome')}</SelectItem>
                    <SelectItem value='success'>{t('monitoring.conditions.success')}</SelectItem>
                    <SelectItem value='failure'>{t('monitoring.conditions.failure')}</SelectItem>
                  </SelectContent>
                </Select>
                <Input
                  type='number'
                  placeholder={t('monitoring.conditions.minFailureCount')}
                  value={profile.conditions?.minFailureCount ?? ''}
                  disabled={!canWrite}
                  onChange={(event) =>
                    updateProfile(profile.id, (current) => ({
                      ...current,
                      conditions: {
                        ...(current.conditions ?? {}),
                        minFailureCount: numericValue(event.target.value),
                      },
                    }))
                  }
                />
                <Input
                  type='number'
                  placeholder={t('monitoring.conditions.balanceGTE')}
                  value={profile.conditions?.balanceGTE ?? ''}
                  disabled={!canWrite}
                  onChange={(event) =>
                    updateProfile(profile.id, (current) => ({
                      ...current,
                      conditions: {
                        ...(current.conditions ?? {}),
                        balanceGTE: numericValue(event.target.value),
                      },
                    }))
                  }
                />
                <Input
                  type='number'
                  placeholder={t('monitoring.conditions.balanceLTE')}
                  value={profile.conditions?.balanceLTE ?? ''}
                  disabled={!canWrite}
                  onChange={(event) =>
                    updateProfile(profile.id, (current) => ({
                      ...current,
                      conditions: {
                        ...(current.conditions ?? {}),
                        balanceLTE: numericValue(event.target.value),
                      },
                    }))
                  }
                />
                <Input
                  placeholder={t('monitoring.conditions.reasonContains')}
                  value={profile.conditions?.reasonContains ?? ''}
                  disabled={!canWrite}
                  onChange={(event) =>
                    updateProfile(profile.id, (current) => ({
                      ...current,
                      conditions: {
                        ...(current.conditions ?? {}),
                        reasonContains: event.target.value || null,
                      },
                    }))
                  }
                />
                {target === 'key' ? (
                  <div className='flex flex-wrap gap-2'>
                    {KEY_STATUSES.map((status) => (
                      <label key={status} className='flex items-center gap-1 text-xs'>
                        <Checkbox
                          checked={(profile.conditions?.keyStatuses ?? []).includes(status)}
                          disabled={!canWrite}
                          onCheckedChange={(checked) =>
                            updateProfile(profile.id, (current) => ({
                              ...current,
                              conditions: {
                                ...(current.conditions ?? {}),
                                keyStatuses: checked
                                  ? [...(current.conditions?.keyStatuses ?? []), status]
                                  : (current.conditions?.keyStatuses ?? []).filter((item) => item !== status),
                              },
                            }))
                          }
                        />
                        {t(`monitoring.keyStatuses.${status}`)}
                      </label>
                    ))}
                  </div>
                ) : null}
              </div>
            </div>

            <div className='space-y-2'>
              <Label>{t('monitoring.profiles.actions')}</Label>
              <div className='space-y-2 rounded-md border p-3'>
                {(profile.actions ?? []).map((action, actionIndex) => (
                  <div key={`${profile.id}-${actionIndex}`} className='flex gap-2'>
                    <Select
                      value={action.type}
                      disabled={!canWrite}
                      onValueChange={(value) =>
                        updateProfile(profile.id, (current) => ({
                          ...current,
                          actions: (current.actions ?? []).map((item, index) =>
                            index === actionIndex ? { ...item, type: value as FailurePolicyActionType } : item
                          ),
                        }))
                      }
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {(target === 'key' ? KEY_ACTIONS : CHANNEL_ACTIONS).map((item) => (
                          <SelectItem key={item} value={item}>
                            {t(`monitoring.actions.${item}`)}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <Button
                      type='button'
                      variant='ghost'
                      size='icon'
                      disabled={!canWrite || (profile.actions ?? []).length <= 1}
                      onClick={() =>
                        updateProfile(profile.id, (current) => ({
                          ...current,
                          actions: (current.actions ?? []).filter((_, index) => index !== actionIndex),
                        }))
                      }
                    >
                      <IconTrash className='h-4 w-4' />
                    </Button>
                  </div>
                ))}
                <Button
                  type='button'
                  variant='outline'
                  size='sm'
                  disabled={!canWrite}
                  onClick={() =>
                    updateProfile(profile.id, (current) => ({
                      ...current,
                      actions: [...(current.actions ?? []), { type: target === 'key' ? 'report_only' : 'report_only', backoff: null }],
                    }))
                  }
                >
                  <IconPlus className='mr-1 h-4 w-4' />
                  {t('monitoring.profiles.addAction')}
                </Button>
              </div>
            </div>
          </div>
        </div>
      ))}
    </section>
  );
}

function MonitoringHistory({ rules, channels }: { rules: MonitoringRule[]; channels: Array<{ id: string; name: string }> }) {
  const { t } = useTranslation();
  const [ruleFilter, setRuleFilter] = useState('all');
  const [channelFilter, setChannelFilter] = useState('all');
  const [outcomeFilter, setOutcomeFilter] = useState<OutcomeFilter>('all');
  const [triggerFilter, setTriggerFilter] = useState('all');
  const [keyFilter, setKeyFilter] = useState('');
  const [after, setAfter] = useState<string | undefined>();
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const pageSize = 20;

  const resetPagination = () => {
    setAfter(undefined);
    setCursorStack([]);
  };

  const where = useMemo(() => {
    const filters: Record<string, unknown> = {};
    if (ruleFilter !== 'all') filters.ruleID = ruleFilter;
    if (channelFilter !== 'all') {
      const channel = channels.find((item) => extractNumberID(item.id) === channelFilter);
      if (channel) filters.channelID = channel.id;
    }
    if (triggerFilter !== 'all') filters.trigger = triggerFilter;
    if (outcomeFilter === 'success') {
      filters.success = true;
      filters.skipped = false;
    } else if (outcomeFilter === 'failure') {
      filters.success = false;
      filters.skipped = false;
    } else if (outcomeFilter === 'skipped') {
      filters.skipped = true;
    }
    if (keyFilter.trim()) {
      filters.or = [{ keyIDContainsFold: keyFilter.trim() }, { maskedKeyContainsFold: keyFilter.trim() }];
    }
    return Object.keys(filters).length > 0 ? filters : undefined;
  }, [channelFilter, channels, keyFilter, outcomeFilter, ruleFilter, triggerFilter]);

  const { data, isFetching } = useMonitoringEvents({
    first: pageSize,
    after,
    orderBy: {
      field: 'CHECKED_AT',
      direction: 'DESC',
    },
    where,
  });

  const rows = data?.edges?.map((edge) => edge.node).filter((node): node is NonNullable<typeof node> => Boolean(node)) ?? [];

  return (
    <Card>
      <CardHeader>
        <div className='flex flex-wrap items-start justify-between gap-3'>
          <div>
            <CardTitle>{t('monitoring.history.title')}</CardTitle>
            <CardDescription>{t('monitoring.history.description')}</CardDescription>
          </div>
          <Badge variant='outline'>{t('monitoring.history.total', { count: data?.totalCount ?? 0 })}</Badge>
        </div>
      </CardHeader>
      <CardContent className='space-y-4'>
        <div className='grid gap-2 md:grid-cols-5'>
          <Select
            value={ruleFilter}
            onValueChange={(value) => {
              setRuleFilter(value);
              resetPagination();
            }}
          >
            <SelectTrigger>
              <SelectValue placeholder={t('monitoring.history.filters.rule')} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value='all'>{t('monitoring.history.filters.allRules')}</SelectItem>
              {rules.map((rule) => (
                <SelectItem key={rule.id} value={rule.id}>
                  {rule.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Select
            value={channelFilter}
            onValueChange={(value) => {
              setChannelFilter(value);
              resetPagination();
            }}
          >
            <SelectTrigger>
              <SelectValue placeholder={t('monitoring.history.filters.channel')} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value='all'>{t('monitoring.history.filters.allChannels')}</SelectItem>
              {channels.map((channel) => (
                <SelectItem key={channel.id} value={extractNumberID(channel.id)}>
                  #{extractNumberID(channel.id)} {channel.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Select
            value={outcomeFilter}
            onValueChange={(value) => {
              setOutcomeFilter(value as OutcomeFilter);
              resetPagination();
            }}
          >
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value='all'>{t('monitoring.history.filters.allOutcomes')}</SelectItem>
              <SelectItem value='success'>{t('monitoring.history.outcomes.success')}</SelectItem>
              <SelectItem value='failure'>{t('monitoring.history.outcomes.failure')}</SelectItem>
              <SelectItem value='skipped'>{t('monitoring.history.outcomes.skipped')}</SelectItem>
            </SelectContent>
          </Select>
          <Select
            value={triggerFilter}
            onValueChange={(value) => {
              setTriggerFilter(value);
              resetPagination();
            }}
          >
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value='all'>{t('monitoring.history.filters.allTriggers')}</SelectItem>
              <SelectItem value='scheduled'>{t('monitoring.triggers.scheduled')}</SelectItem>
              <SelectItem value='manual'>{t('monitoring.triggers.manual')}</SelectItem>
              <SelectItem value='request'>{t('monitoring.triggers.request')}</SelectItem>
              <SelectItem value='monitor_rule'>{t('monitoring.triggers.monitor_rule')}</SelectItem>
            </SelectContent>
          </Select>
          <Input
            value={keyFilter}
            placeholder={t('monitoring.history.filters.key')}
            onChange={(event) => {
              setKeyFilter(event.target.value);
              resetPagination();
            }}
          />
        </div>

        <div className='overflow-hidden rounded-md border'>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t('monitoring.history.columns.checkedAt')}</TableHead>
                <TableHead>{t('monitoring.history.columns.outcome')}</TableHead>
                <TableHead>{t('monitoring.history.columns.rule')}</TableHead>
                <TableHead>{t('monitoring.history.columns.channel')}</TableHead>
                <TableHead>{t('monitoring.history.columns.key')}</TableHead>
                <TableHead>{t('monitoring.history.columns.probe')}</TableHead>
                <TableHead>{t('monitoring.history.columns.action')}</TableHead>
                <TableHead>{t('monitoring.history.columns.reason')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((row) => (
                <TableRow key={row.id}>
                  <TableCell className='text-xs whitespace-nowrap'>{formatDate(row.checkedAt)}</TableCell>
                  <TableCell>
                    <Badge variant={row.skipped ? 'secondary' : row.success ? 'default' : 'destructive'} className='gap-1'>
                      {row.skipped ? (
                        t('monitoring.history.outcomes.skipped')
                      ) : row.success ? (
                        <>
                          <IconCircleCheck className='h-3 w-3' />
                          {t('monitoring.history.outcomes.success')}
                        </>
                      ) : (
                        <>
                          <IconCircleX className='h-3 w-3' />
                          {t('monitoring.history.outcomes.failure')}
                        </>
                      )}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <div className='max-w-44 truncate'>{row.ruleName || row.ruleID || '-'}</div>
                    <div className='text-muted-foreground text-xs'>{row.trigger}</div>
                  </TableCell>
                  <TableCell>
                    <div className='max-w-44 truncate'>{row.channelName || '-'}</div>
                    <div className='text-muted-foreground font-mono text-xs'>#{extractNumberID(row.channelID)}</div>
                  </TableCell>
                  <TableCell>
                    <div className='font-mono text-xs'>{row.maskedKey || '-'}</div>
                    <div className='text-muted-foreground max-w-36 truncate font-mono text-xs'>{row.keyID || '-'}</div>
                  </TableCell>
                  <TableCell>{row.probe || '-'}</TableCell>
                  <TableCell>{row.action ? formatActionLabel(row.action) : '-'}</TableCell>
                  <TableCell>
                    <div className='max-w-64 truncate' title={row.reason ?? undefined}>
                      {row.reason || '-'}
                    </div>
                  </TableCell>
                </TableRow>
              ))}
              {rows.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={8} className='text-muted-foreground h-24 text-center'>
                    {isFetching ? t('common.loading') : t('monitoring.history.empty')}
                  </TableCell>
                </TableRow>
              ) : null}
            </TableBody>
          </Table>
        </div>

        <div className='flex items-center justify-between'>
          <div className='text-muted-foreground text-sm'>
            {isFetching
              ? t('common.loading')
              : t('pagination.selectedInfoWithTotal', {
                  selectedRows: rows.length,
                  dataLength: pageSize,
                  totalCount: data?.totalCount ?? 0,
                })}
          </div>
          <div className='flex gap-2'>
            <Button
              variant='outline'
              disabled={cursorStack.length === 0 || isFetching}
              onClick={() => {
                const nextStack = [...cursorStack];
                const previous = nextStack.pop();
                setCursorStack(nextStack);
                setAfter(previous);
              }}
            >
              {t('pagination.previousPage')}
            </Button>
            <Button
              variant='outline'
              disabled={!data?.pageInfo?.hasNextPage || !data.pageInfo.endCursor || isFetching}
              onClick={() => {
                setCursorStack((current) => [...current, after]);
                setAfter(data?.pageInfo?.endCursor ?? undefined);
              }}
            >
              {t('pagination.nextPage')}
            </Button>
          </div>
        </div>
      </CardContent>
    </Card>
  );
}

export default function MonitoringPage() {
  return <MonitoringManagement />;
}
