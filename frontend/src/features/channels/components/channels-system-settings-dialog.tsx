'use client';

import React, { useCallback } from 'react';
import { Loader2, Settings2, Activity } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import {
  useChannelSetting,
  useUpdateChannelSetting,
  type AutoSyncFrequency,
  type ChannelAdvancedActionMenuMode,
  type ChannelKeySelectionStrategy,
  type ProbeFrequency,
} from '@/features/system/data/system';
import { useChannels } from '../context/channels-context';

const PROBE_FREQUENCY_OPTIONS: { value: ProbeFrequency; label: string }[] = [
  { value: 'ONE_MINUTE', label: '1 minute' },
  { value: 'FIVE_MINUTES', label: '5 minutes' },
  { value: 'THIRTY_MINUTES', label: '30 minutes' },
  { value: 'ONE_HOUR', label: '1 hour' },
];

const AUTO_SYNC_FREQUENCY_OPTIONS: { value: AutoSyncFrequency; label: string }[] = [
  { value: 'ONE_HOUR', label: '1 hour' },
  { value: 'SIX_HOURS', label: '6 hours' },
  { value: 'ONE_DAY', label: '1 day' },
];

const ACTION_MENU_MODE_OPTIONS: ChannelAdvancedActionMenuMode[] = ['GROUPED', 'EXPANDED'];
const ROUTING_STRATEGY_OPTIONS: ChannelKeySelectionStrategy[] = ['trace_sticky', 'cache_affinity', 'random', 'round_robin'];
const DEFAULT_LIKELY_AFFINITY_TTL_MINUTES = 30;
const DEFAULT_EXACT_AFFINITY_TTL_MINUTES = 1440;

function normalizeTTL(value: number, fallback: number) {
  return Number.isFinite(value) && value > 0 ? Math.round(value) : fallback;
}

export function ChannelsSystemSettingsDialog() {
  const { t } = useTranslation();
  const { open, setOpen } = useChannels();
  const { data: settings, isLoading } = useChannelSetting();
  const updateSettings = useUpdateChannelSetting();

  const isOpen = open === 'channelSettings';

  const [probeEnabled, setProbeEnabled] = React.useState(false);
  const [probeFrequency, setProbeFrequency] = React.useState<ProbeFrequency>('ONE_MINUTE');
  const [autoSyncFrequency, setAutoSyncFrequency] = React.useState<AutoSyncFrequency>('ONE_HOUR');
  const [advancedActionMenuMode, setAdvancedActionMenuMode] = React.useState<ChannelAdvancedActionMenuMode>('GROUPED');
  const [routingStrategy, setRoutingStrategy] = React.useState<ChannelKeySelectionStrategy>('trace_sticky');
  const [likelyAffinityTTLMinutes, setLikelyAffinityTTLMinutes] = React.useState(DEFAULT_LIKELY_AFFINITY_TTL_MINUTES);
  const [exactAffinityTTLMinutes, setExactAffinityTTLMinutes] = React.useState(DEFAULT_EXACT_AFFINITY_TTL_MINUTES);

  React.useEffect(() => {
    if (settings?.probe) {
      setProbeEnabled(settings.probe.enabled);
      setProbeFrequency(settings.probe.frequency);
    }
    if (settings?.autoSync?.frequency) {
      setAutoSyncFrequency(settings.autoSync.frequency);
    }
    if (settings?.actionMenu?.advancedActionsMode) {
      setAdvancedActionMenuMode(settings.actionMenu.advancedActionsMode);
    }
    if (settings?.routing?.strategy) {
      setRoutingStrategy(settings.routing.strategy);
      setLikelyAffinityTTLMinutes(settings.routing.likelyAffinityTTLMinutes ?? DEFAULT_LIKELY_AFFINITY_TTL_MINUTES);
      setExactAffinityTTLMinutes(settings.routing.exactAffinityTTLMinutes ?? DEFAULT_EXACT_AFFINITY_TTL_MINUTES);
    }
  }, [settings]);

  const handleSave = useCallback(async () => {
    await updateSettings.mutateAsync({
      probe: {
        enabled: probeEnabled,
        frequency: probeFrequency,
      },
      autoSync: {
        frequency: autoSyncFrequency,
      },
      actionMenu: {
        advancedActionsMode: advancedActionMenuMode,
      },
      routing: {
        strategy: routingStrategy,
        likelyAffinityTTLMinutes:
          routingStrategy === 'cache_affinity' ? normalizeTTL(likelyAffinityTTLMinutes, DEFAULT_LIKELY_AFFINITY_TTL_MINUTES) : null,
        exactAffinityTTLMinutes:
          routingStrategy === 'cache_affinity' ? normalizeTTL(exactAffinityTTLMinutes, DEFAULT_EXACT_AFFINITY_TTL_MINUTES) : null,
      },
    });
    setOpen(null);
  }, [
    updateSettings,
    probeEnabled,
    probeFrequency,
    autoSyncFrequency,
    advancedActionMenuMode,
    routingStrategy,
    likelyAffinityTTLMinutes,
    exactAffinityTTLMinutes,
    setOpen,
  ]);

  const handleClose = useCallback(() => {
    setOpen(null);
  }, [setOpen]);

  return (
    <Dialog open={isOpen} onOpenChange={handleClose}>
      <DialogContent className='sm:max-w-[720px]'>
        <DialogHeader>
          <DialogTitle className='flex items-center gap-2'>
            <Settings2 className='h-5 w-5' />
            {t('channels.dialogs.systemSettings.title')}
          </DialogTitle>
          <DialogDescription>{t('channels.dialogs.systemSettings.description')}</DialogDescription>
        </DialogHeader>

        {isLoading ? (
          <div className='flex items-center justify-center py-12'>
            <Loader2 className='h-8 w-8 animate-spin' />
          </div>
        ) : (
          <div className='space-y-4'>
            <Card>
              <CardHeader className='pb-0'>
                <CardTitle className='flex items-center gap-2 text-sm'>
                  <Activity className='text-muted-foreground h-4 w-4' />
                  {t('channels.dialogs.systemSettings.channelProbe.label')}
                </CardTitle>
              </CardHeader>
              <CardContent className='space-y-4 pt-4'>
                <div className='flex items-center justify-between'>
                  <div className='flex-1 pr-4'>
                    <p className='text-sm font-medium'>{t('channels.dialogs.systemSettings.channelProbe.enabledLabel')}</p>
                    <p className='text-muted-foreground text-sm'>{t('channels.dialogs.systemSettings.channelProbe.enabledDescription')}</p>
                    <p className='text-muted-foreground mt-1 text-xs'>
                      {t('channels.dialogs.systemSettings.channelProbe.probeDescription')}
                    </p>
                  </div>
                  <Switch id='probe-enabled' checked={probeEnabled} onCheckedChange={setProbeEnabled} disabled={updateSettings.isPending} />
                </div>

                {probeEnabled && (
                  <div className='space-y-2'>
                    <label htmlFor='probe-frequency' className='text-sm font-medium'>
                      {t('channels.dialogs.systemSettings.channelProbe.frequencyLabel')}
                    </label>
                    <Select value={probeFrequency} onValueChange={(value) => setProbeFrequency(value as ProbeFrequency)}>
                      <SelectTrigger id='probe-frequency' disabled={updateSettings.isPending}>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {PROBE_FREQUENCY_OPTIONS.map((option) => (
                          <SelectItem key={option.value} value={option.value}>
                            {option.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <p className='text-muted-foreground text-xs'>
                      {t('channels.dialogs.systemSettings.channelProbe.frequencyDescription')}
                    </p>
                    <p className='text-muted-foreground mt-1 text-xs'>
                      {t('channels.dialogs.systemSettings.channelProbe.frequencyWarning')}
                    </p>
                  </div>
                )}
              </CardContent>
            </Card>
            <Card>
              <CardHeader className='pb-0'>
                <CardTitle className='flex items-center gap-2 text-sm'>
                  <Activity className='text-muted-foreground h-4 w-4' />
                  {t('channels.dialogs.systemSettings.autoSync.label')}
                </CardTitle>
              </CardHeader>
              <CardContent className='space-y-4 pt-4'>
                <div className='space-y-2'>
                  <label htmlFor='auto-sync-frequency' className='text-sm font-medium'>
                    {t('channels.dialogs.systemSettings.autoSync.frequencyLabel')}
                  </label>
                  <Select value={autoSyncFrequency} onValueChange={(value) => setAutoSyncFrequency(value as AutoSyncFrequency)}>
                    <SelectTrigger id='auto-sync-frequency' disabled={updateSettings.isPending}>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {AUTO_SYNC_FREQUENCY_OPTIONS.map((option) => (
                        <SelectItem key={option.value} value={option.value}>
                          {option.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <p className='text-muted-foreground text-xs'>{t('channels.dialogs.systemSettings.autoSync.frequencyDescription')}</p>
                </div>
              </CardContent>
            </Card>
            <Card>
              <CardHeader className='pb-0'>
                <CardTitle className='flex items-center gap-2 text-sm'>
                  <Settings2 className='text-muted-foreground h-4 w-4' />
                  {t('channels.dialogs.systemSettings.actionMenu.label')}
                </CardTitle>
              </CardHeader>
              <CardContent className='space-y-4 pt-4'>
                <div className='space-y-2'>
                  <label htmlFor='action-menu-mode' className='text-sm font-medium'>
                    {t('channels.dialogs.systemSettings.actionMenu.modeLabel')}
                  </label>
                  <Select
                    value={advancedActionMenuMode}
                    onValueChange={(value) => setAdvancedActionMenuMode(value as ChannelAdvancedActionMenuMode)}
                  >
                    <SelectTrigger id='action-menu-mode' disabled={updateSettings.isPending}>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {ACTION_MENU_MODE_OPTIONS.map((option) => (
                        <SelectItem key={option} value={option}>
                          {t(`channels.dialogs.systemSettings.actionMenu.modes.${option}.label`)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <p className='text-muted-foreground text-xs'>
                    {t(`channels.dialogs.systemSettings.actionMenu.modes.${advancedActionMenuMode}.description`)}
                  </p>
                </div>
              </CardContent>
            </Card>
            <Card>
              <CardHeader className='pb-0'>
                <CardTitle className='flex items-center gap-2 text-sm'>
                  <Settings2 className='text-muted-foreground h-4 w-4' />
                  {t('channels.dialogs.systemSettings.routing.label')}
                </CardTitle>
              </CardHeader>
              <CardContent className='space-y-4 pt-4'>
                <div className='space-y-2'>
                  <label htmlFor='global-routing-strategy' className='text-sm font-medium'>
                    {t('channels.dialogs.systemSettings.routing.strategyLabel')}
                  </label>
                  <Select value={routingStrategy} onValueChange={(value) => setRoutingStrategy(value as ChannelKeySelectionStrategy)}>
                    <SelectTrigger id='global-routing-strategy' disabled={updateSettings.isPending}>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {ROUTING_STRATEGY_OPTIONS.map((option) => (
                        <SelectItem key={option} value={option}>
                          {t(`channels.dialogs.keyRouting.strategies.${option}.label`)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <p className='text-muted-foreground text-xs'>{t('channels.dialogs.systemSettings.routing.strategyDescription')}</p>
                </div>
                {routingStrategy === 'cache_affinity' ? (
                  <div className='grid gap-4 sm:grid-cols-2'>
                    <div className='space-y-2'>
                      <label htmlFor='global-likely-affinity-ttl' className='text-sm font-medium'>
                        {t('channels.dialogs.keyRouting.fields.likelyAffinityTTLMinutes.label')}
                      </label>
                      <Input
                        id='global-likely-affinity-ttl'
                        type='number'
                        min={1}
                        max={1440}
                        value={likelyAffinityTTLMinutes}
                        onChange={(event) => setLikelyAffinityTTLMinutes(Number(event.target.value))}
                        disabled={updateSettings.isPending}
                      />
                      <p className='text-muted-foreground text-xs'>
                        {t('channels.dialogs.keyRouting.fields.likelyAffinityTTLMinutes.description')}
                      </p>
                    </div>
                    <div className='space-y-2'>
                      <label htmlFor='global-exact-affinity-ttl' className='text-sm font-medium'>
                        {t('channels.dialogs.keyRouting.fields.exactAffinityTTLMinutes.label')}
                      </label>
                      <Input
                        id='global-exact-affinity-ttl'
                        type='number'
                        min={1}
                        max={10080}
                        value={exactAffinityTTLMinutes}
                        onChange={(event) => setExactAffinityTTLMinutes(Number(event.target.value))}
                        disabled={updateSettings.isPending}
                      />
                      <p className='text-muted-foreground text-xs'>
                        {t('channels.dialogs.keyRouting.fields.exactAffinityTTLMinutes.description')}
                      </p>
                    </div>
                  </div>
                ) : null}
              </CardContent>
            </Card>
          </div>
        )}

        <DialogFooter>
          <Button variant='outline' onClick={handleClose} disabled={updateSettings.isPending}>
            {t('common.buttons.cancel')}
          </Button>
          <Button onClick={handleSave} disabled={updateSettings.isPending || isLoading}>
            {updateSettings.isPending ? (
              <>
                <Loader2 className='mr-2 h-4 w-4 animate-spin' />
                {t('common.buttons.saving')}
              </>
            ) : (
              t('common.buttons.save')
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
