import { useEffect, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Skeleton } from '@/components/ui/skeleton';
import { TagsAutocompleteInput } from '@/components/ui/tags-autocomplete-input';
import {
  KEY_FIELDS,
  KEYS_QUERY,
  MODELS_QUERY,
  DASHBOARD_QUERY,
  portalRequest,
  usePortalQuery,
  type PortalKey,
  type PortalKeyInput,
  type PortalModel,
  type PortalDashboard,
} from './data';
import { PortalEmpty, PortalError, usePortalGroup } from './layout';
import { KeyQuota } from './overview';
import { PortalTable } from './table';

function KeyEditor({ item, onClose }: { item: PortalKey | null; onClose: () => void }) {
  const { t } = useTranslation();
  const group = usePortalGroup()!;
  const queryClient = useQueryClient();
  const [name, setName] = useState(item?.name || '');
  const [models, setModels] = useState(item?.modelIDs || []);
  const [ips, setIPs] = useState(item?.allowedIPs.join(', ') || '');
  const [container, setContainer] = useState<HTMLDivElement | null>(null);
  const available = usePortalQuery<{ models: PortalModel[] }>('models', MODELS_QUERY, { projectID: group.id });
  const save = useMutation({
    mutationFn: async () => {
      const input: PortalKeyInput = { name, modelIDs: models, allowedIPs: ips.split(/[\s,]+/).filter(Boolean) };
      return item
        ? portalRequest(`mutation Update($id: Int!, $input: PortalKeyInput!) { updateKey(id: $id, input: $input) { ${KEY_FIELDS} } }`, {
            id: item.id,
            input,
          })
        : portalRequest(
            `mutation Create($projectID: Int!, $input: PortalKeyInput!) { createKey(projectID: $projectID, input: $input) { ${KEY_FIELDS} } }`,
            { projectID: group.id, input }
          );
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['portal'] });
      onClose();
      toast.success(t('portal.saved'));
    },
  });
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent ref={setContainer}>
        <DialogHeader>
          <DialogTitle>{t(item ? 'portal.editKey' : 'portal.createKey')}</DialogTitle>
          <DialogDescription>{t('portal.followGroup')}</DialogDescription>
        </DialogHeader>
        <form
          className='space-y-4'
          onSubmit={(e) => {
            e.preventDefault();
            save.mutate();
          }}
        >
          <div className='space-y-2'>
            <Label htmlFor='key-name'>{t('portal.name')}</Label>
            <Input id='key-name' required maxLength={128} value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div className='space-y-2'>
            <Label>{t('portal.allowedModels')}</Label>
            <TagsAutocompleteInput
              value={models}
              onChange={setModels}
              suggestions={available.data?.models.map((m) => m.id) || []}
              portalContainer={container}
            />
          </div>
          <div className='space-y-2'>
            <Label htmlFor='key-ips'>{t('portal.allowedIPs')}</Label>
            <Input id='key-ips' value={ips} onChange={(e) => setIPs(e.target.value)} placeholder='192.0.2.0/24, 2001:db8::/32' />
          </div>
          {save.error && (
            <p role='alert' className='text-destructive text-sm'>
              {save.error.message}
            </p>
          )}
          <DialogFooter>
            <Button type='button' variant='outline' onClick={onClose}>
              {t('portal.cancel')}
            </Button>
            <Button type='submit' disabled={save.isPending || !name.trim()}>
              {t('portal.save')}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
function KeyHistory({ item, onClose }: { item: PortalKey; onClose: () => void }) {
  const { t } = useTranslation();
  const stats = usePortalQuery<{ dashboard: PortalDashboard }>('key-dashboard', DASHBOARD_QUERY, {
    projectID: item.projectID,
    keyID: item.id,
    period: 'all',
  });
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>
            {item.name}: {t('portal.details')}
          </DialogTitle>
          <DialogDescription>{t('portal.allRetained')}</DialogDescription>
        </DialogHeader>
        {stats.error ? (
          <PortalError error={stats.error} retry={() => void stats.refetch()} />
        ) : stats.data ? (
          <PortalTable
            data={stats.data.dashboard.models}
            columns={[
              { accessorKey: 'modelID', header: t('portal.model') },
              { accessorKey: 'totalTokens', header: 'Token' },
              { accessorKey: 'usageCount', header: t('portal.tokens') },
            ]}
          />
        ) : (
          <Skeleton className='h-32 w-full' />
        )}
      </DialogContent>
    </Dialog>
  );
}
export default function PortalKeys() {
  const { t } = useTranslation();
  const group = usePortalGroup();
  const queryClient = useQueryClient();
  const [after, setAfter] = useState<number | null>(null);
  const [previous, setPrevious] = useState<PortalKey[]>([]);
  const [editor, setEditor] = useState<PortalKey | null | undefined>(undefined);
  const [history, setHistory] = useState<PortalKey | null>(null);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  const page = usePortalQuery<{ keys: { items: PortalKey[]; totalCount: number; nextCursor: number | null } }>(
    'keys',
    KEYS_QUERY,
    { projectID: group?.id, first: 50, after },
    !!group
  );
  const rows = after === null ? page.data?.keys.items || [] : [...previous, ...(page.data?.keys.items || [])];
  const action = useMutation({
    mutationFn: ({ id, status, rotate }: { id: number; status?: string; rotate?: boolean }) =>
      rotate
        ? portalRequest(`mutation Rotate($id: Int!) { rotateKey(id: $id) { ${KEY_FIELDS} } }`, { id })
        : portalRequest(`mutation Status($id: Int!, $status: String!) { setKeyStatus(id: $id, status: $status) { ${KEY_FIELDS} } }`, {
            id,
            status,
          }),
    onSuccess: () => {
      setAfter(null);
      void queryClient.invalidateQueries({ queryKey: ['portal'] });
    },
  });
  async function copyKey(id: number) {
    try {
      const result = await portalRequest<{ revealKey: string }>('mutation Reveal($id: Int!) { revealKey(id: $id) }', { id });
      if (!mounted.current) return;
      await navigator.clipboard.writeText(result.revealKey);
      toast.success(t('portal.copied'));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t('portal.copyFailed'));
    }
  }
  if (!group) return <PortalEmpty title={t('portal.noGroups')} description={t('portal.noGroupsHelp')} />;
  return (
    <div className='space-y-6'>
      <div className='flex flex-wrap items-center justify-between gap-3'>
        <h1 className='text-2xl font-semibold'>{t('portal.keys')}</h1>
        <Button disabled={!group.callable} onClick={() => setEditor(null)}>
          {t('portal.createKey')}
        </Button>
      </div>
      {action.error && (
        <p role='alert' className='text-destructive text-sm'>
          {action.error.message}
        </p>
      )}
      {page.error ? (
        <PortalError error={page.error} retry={() => void page.refetch()} />
      ) : page.isLoading ? (
        <Skeleton className='h-64 w-full' />
      ) : rows.length ? (
        <PortalTable
          data={rows}
          columns={[
            {
              id: 'name',
              header: t('portal.name'),
              cell: ({ row }) => (
                <div>
                  <p className='font-medium'>{row.original.name}</p>
                  <code className='text-muted-foreground text-xs'>{row.original.maskedKey}</code>
                </div>
              ),
            },
            { id: 'status', header: t('portal.status'), cell: ({ row }) => t(`portal.${row.original.status}`) },
            { id: 'quota', header: t('portal.quota'), cell: ({ row }) => <KeyQuota item={row.original} /> },
            {
              id: 'actions',
              header: t('portal.actions'),
              cell: ({ row }) => {
                const k = row.original;
                return (
                  <div className='flex max-w-80 flex-wrap gap-1'>
                    <Button
                      size='sm'
                      variant='ghost'
                      disabled={!group.callable || k.status !== 'enabled'}
                      onClick={() => void copyKey(k.id)}
                    >
                      {t('portal.copyKey')}
                    </Button>
                    <Button size='sm' variant='ghost' disabled={!group.callable} onClick={() => setEditor(k)}>
                      {t('portal.editKey')}
                    </Button>
                    <Button
                      size='sm'
                      variant='ghost'
                      disabled={action.isPending || (k.status !== 'enabled' && !group.callable)}
                      onClick={() => action.mutate({ id: k.id, status: k.status === 'enabled' ? 'disabled' : 'enabled' })}
                    >
                      {t(k.status === 'enabled' ? 'portal.disable' : 'portal.enable')}
                    </Button>
                    <Button
                      size='sm'
                      variant='ghost'
                      disabled={!group.member || action.isPending}
                      onClick={() => {
                        if (window.confirm(t('portal.rotateConfirm'))) action.mutate({ id: k.id, rotate: true });
                      }}
                    >
                      {t('portal.rotate')}
                    </Button>
                    <Button
                      size='sm'
                      variant='ghost'
                      disabled={k.status === 'archived' || action.isPending}
                      onClick={() => {
                        if (window.confirm(t('portal.archiveConfirm'))) action.mutate({ id: k.id, status: 'archived' });
                      }}
                    >
                      {t('portal.archive')}
                    </Button>
                    <Button size='sm' variant='ghost' onClick={() => setHistory(k)}>
                      {t('portal.details')}
                    </Button>
                  </div>
                );
              },
            },
          ]}
        />
      ) : (
        <PortalEmpty title={t('portal.noKeys')} description={t('portal.noKeysHelp')} />
      )}
      {page.data?.keys.nextCursor && (
        <Button
          variant='outline'
          disabled={page.isFetching}
          onClick={() => {
            setPrevious(rows);
            setAfter(page.data!.keys.nextCursor);
          }}
        >
          {t('portal.more')}
        </Button>
      )}
      {editor !== undefined && (
        <KeyEditor
          item={editor}
          onClose={() => {
            setAfter(null);
            setPrevious([]);
            setEditor(undefined);
          }}
        />
      )}
      {history && <KeyHistory item={history} onClose={() => setHistory(null)} />}
    </div>
  );
}
