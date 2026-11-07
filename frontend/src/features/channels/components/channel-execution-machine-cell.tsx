import { useState } from 'react';
import { nanoid } from 'nanoid';
import { useTranslation } from 'react-i18next';
import { extractNumberID } from '@/lib/utils';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { useChannels } from '../context/channels-context';
import { useChannelExecutionInventory } from '../data/channels';
import { placementSyncState, usePlacementObservation, usePlacementPreview, useSavePlacement } from '../data/placement';
import { Channel } from '../data/schema';

export function ChannelPlacementDialog({ channel, onClose }: { channel: Channel; onClose: () => void }) {
  const { t } = useTranslation();
  const channelID = Number(extractNumberID(channel.id));
  const { data: inventory } = useChannelExecutionInventory();
  const [target, setTarget] = useState('');
  const [requestID, setRequestID] = useState(() => nanoid());
  const observation = usePlacementObservation(channelID, true);
  const value = observation.data;
  const currentOwner = value?.owner ?? channel.executionMachine;
  const assigning = !channel.executionMachine;
  const savedThisRequest = value?.change?.request_id === requestID;
  const preview = usePlacementPreview(channelID, target, !savedThisRequest);
  const save = useSavePlacement(channelID);
  const { targetReady, workerReady } = placementSyncState(value);
  const error = !savedThisRequest && (save.error ?? preview.error ?? observation.error);

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose(); }}>
      <DialogContent className='max-h-[90vh] overflow-y-auto sm:max-w-lg' data-testid='channel-placement-dialog'>
        <DialogHeader>
          <DialogTitle>{t(assigning ? 'channels.placement.assignTitle' : 'channels.placement.changeTitle')}</DialogTitle>
          <DialogDescription>{channel.name}</DialogDescription>
        </DialogHeader>
        <div className='space-y-4'>
          <dl className='grid grid-cols-[auto_1fr] items-center gap-x-6 gap-y-2 text-sm'>
            <dt className='text-muted-foreground'>{t('channels.placement.currentOwner')}</dt>
            <dd className='font-mono' data-testid='placement-current-owner'>{currentOwner || t('channels.placement.unassigned')}</dd>
            {!savedThisRequest && (
              <>
                <dt className='text-muted-foreground'>{t('channels.placement.targetOwner')}</dt>
                <dd>
                  <Select
                    value={target}
                    onValueChange={(next) => {
                      setTarget(next);
                      setRequestID(nanoid());
                      save.reset();
                    }}
                    disabled={save.isPending}
                  >
                    <SelectTrigger aria-label={t('channels.placement.targetOwner')} data-testid='placement-target'>
                      <SelectValue placeholder={t('channels.placement.chooseTarget')} />
                    </SelectTrigger>
                    <SelectContent>
                      {inventory?.machines.filter((machine) => machine !== currentOwner).map((machine) => (
                        <SelectItem key={machine} value={machine}>{machine}</SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </dd>
              </>
            )}
          </dl>
          {preview.data && !savedThisRequest && (
            <div className='rounded-lg border p-3 text-sm' data-testid='placement-impact'>
              <p className='mb-2 font-medium'>{preview.data.channels.length === 1 ? t('channels.placement.affectedChannel') : t('channels.placement.affectedChannels', { count: preview.data.channels.length })}</p>
              <ul className='max-h-44 space-y-1 overflow-y-auto'>
                {preview.data.channels.map((peer) => (
                  <li key={peer.id} className='flex justify-between gap-4'>
                    <span>{peer.name} <span className='text-muted-foreground'>#{peer.id}</span></span>
                    <span className='text-muted-foreground shrink-0'>{t(`channels.status.${peer.status}`)}</span>
                  </li>
                ))}
              </ul>
            </div>
          )}
          {!savedThisRequest && <p className='text-muted-foreground text-sm'>{t(assigning ? 'channels.placement.assignHelp' : 'channels.placement.onlineHelp')}</p>}
          {value?.change && (
            <div role='status' className='bg-muted/50 space-y-2 rounded-lg p-3 text-sm' data-testid='placement-sync-status'>
              <p className='font-medium'>{t('channels.placement.saved', { owner: value.owner })}</p>
              <p>{t(targetReady ? 'channels.placement.targetReady' : 'channels.placement.targetSyncing')}</p>
              <p>{t(workerReady ? 'channels.placement.workerReady' : 'channels.placement.workerPending')}</p>
              {workerReady && <p className='text-muted-foreground break-all font-mono text-xs'>{value.worker_ack?.worker_revision}</p>}
            </div>
          )}
          {error && <p role='alert' className='text-destructive text-sm'>{error.message}</p>}
        </div>
        <DialogFooter>
          <Button variant={savedThisRequest ? 'default' : 'outline'} onClick={onClose}>{t('common.buttons.close')}</Button>
          {!savedThisRequest && (
            <Button
              data-testid='placement-save'
              disabled={save.isPending || observation.isFetching || preview.isFetching || preview.isError || !preview.data || !target || target === currentOwner}
              onClick={() => {
                if (!preview.data) return;
                save.mutate({
                  target,
                  expected_version: preview.data.placement_version,
                  request_id: requestID,
                  channel_ids: preview.data.channels.map((peer) => peer.id),
                });
              }}
            >
              {save.isPending ? t('channels.placement.preparing') : t(assigning ? 'channels.placement.assignTitle' : 'channels.placement.save')}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function ChannelExecutionMachineCell({ channel, clusterEnabled, editable }: { channel: Channel; clusterEnabled?: boolean; editable: boolean }) {
  const { t } = useTranslation();
  const { setCurrentRow, setOpen } = useChannels();
  const label = channel.executionMachine ?? (clusterEnabled === undefined ? '—' : t(clusterEnabled ? 'channels.placement.unassigned' : 'channels.placement.standalone'));
  return (
    <div className='flex items-center justify-center gap-1'>
      <span data-testid='channel-execution-machine' className={channel.executionMachine ? 'font-mono text-sm' : 'text-muted-foreground text-sm'}>{label}</span>
      {editable && (channel.executionMachine || channel.status === 'disabled') && (
        <Button
          variant='ghost'
          size='sm'
          className='text-muted-foreground h-6 px-2 text-xs'
          data-testid='channel-placement-change'
          onClick={() => {
            setCurrentRow(channel);
            setOpen('placement');
          }}
        >
          {t(channel.executionMachine ? 'channels.placement.change' : 'channels.placement.assign')}
        </Button>
      )}
    </div>
  );
}
