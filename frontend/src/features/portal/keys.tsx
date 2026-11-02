import { useEffect, useRef, useState } from 'react';
import { format } from 'date-fns';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { IconArchive, IconCheck, IconEdit, IconRefresh, IconUserCheck, IconUserOff } from '@tabler/icons-react';
import { BarChart3 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { DropdownMenuItem, DropdownMenuSeparator } from '@/components/ui/dropdown-menu';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Skeleton } from '@/components/ui/skeleton';
import { TagsAutocompleteInput } from '@/components/ui/tags-autocomplete-input';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { KeyCredentialCell } from '@/components/key-credential-cell';
import LongText from '@/components/long-text';
import { RowActionsMenu } from '@/components/row-actions-menu';
import { ServerSidePagination } from '@/components/server-side-pagination';
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
      <DialogContent ref={setContainer} aria-describedby={undefined}>
        <DialogHeader>
          <DialogTitle>{t(item ? 'portal.editKey' : 'portal.createKey')}</DialogTitle>
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
            <p className='text-muted-foreground text-xs'>{t('portal.followGroup')}</p>
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
              { accessorKey: 'inputTokens', header: t('portal.input') },
              { accessorKey: 'outputTokens', header: t('portal.output') },
              { accessorKey: 'totalTokens', header: 'Token' },
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
  const [cursors, setCursors] = useState<(number | null)[]>([null]);
  const [pageSize, setPageSize] = useState(20);
  const [confirm, setConfirm] = useState<{ key: PortalKey; rotate?: boolean } | null>(null);
  const after = cursors[cursors.length - 1];
  const resetPage = () => setCursors([null]);
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
    { projectID: group?.id, first: pageSize, after },
    !!group
  );
  const rows = page.data?.keys.items || [];
  const action = useMutation({
    mutationFn: ({ id, status, rotate }: { id: number; status?: string; rotate?: boolean }) =>
      rotate
        ? portalRequest(`mutation Rotate($id: Int!) { rotateKey(id: $id) { ${KEY_FIELDS} } }`, { id })
        : portalRequest(`mutation Status($id: Int!, $status: String!) { setKeyStatus(id: $id, status: $status) { ${KEY_FIELDS} } }`, {
            id,
            status,
          }),
    onSuccess: () => {
      resetPage();
      setConfirm(null);
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
              accessorKey: 'name',
              header: t('portal.name'),
              cell: ({ row }) => <LongText className='max-w-36 font-medium'>{row.original.name}</LongText>,
            },
            {
              id: 'key',
              header: t('apikeys.columns.key'),
              cell: ({ row }) => (
                <KeyCredentialCell
                  maskedKey={row.original.maskedKey}
                  onCopy={() => copyKey(row.original.id)}
                  disabled={!group.callable || row.original.status !== 'enabled'}
                />
              ),
            },
            {
              id: 'status',
              header: t('portal.status'),
              cell: ({ row }) => (
                <span
                  className={
                    row.original.status === 'enabled'
                      ? 'text-green-600'
                      : row.original.status === 'archived'
                        ? 'text-orange-600'
                        : 'text-red-600'
                  }
                >
                  {t(`portal.${row.original.status}`)}
                </span>
              ),
            },
            { id: 'quota', header: t('portal.quota'), cell: ({ row }) => <KeyQuota item={row.original} /> },
            {
              id: 'createdAt',
              header: t('common.columns.createdAt'),
              cell: ({ row }) => (
                <span className='text-muted-foreground whitespace-nowrap'>
                  {format(new Date(row.original.createdAt), 'yyyy-MM-dd HH:mm')}
                </span>
              ),
            },
            {
              id: 'actions',
              header: t('portal.actions'),
              cell: ({ row }) => {
                const k = row.original;
                return (
                  <RowActionsMenu>
                    <DropdownMenuItem onSelect={() => setHistory(k)}>
                      <BarChart3 className='mr-2 h-4 w-4' />
                      {t('portal.details')}
                    </DropdownMenuItem>
                    <DropdownMenuSeparator />
                    <DropdownMenuItem disabled={!group.callable} onSelect={() => setEditor(k)}>
                      <IconEdit className='mr-2 h-4 w-4' />
                      {t('common.actions.edit')}
                    </DropdownMenuItem>
                    {k.status !== 'archived' && (
                      <DropdownMenuItem
                        disabled={action.isPending || (k.status !== 'enabled' && !group.callable)}
                        onSelect={() => action.mutate({ id: k.id, status: k.status === 'enabled' ? 'disabled' : 'enabled' })}
                        className={k.status === 'enabled' ? 'text-orange-600' : 'text-green-600'}
                      >
                        {k.status === 'enabled' ? <IconUserOff className='mr-2 h-4 w-4' /> : <IconUserCheck className='mr-2 h-4 w-4' />}
                        {t(k.status === 'enabled' ? 'portal.disable' : 'portal.enable')}
                      </DropdownMenuItem>
                    )}
                    <DropdownMenuItem
                      disabled={action.isPending || (k.status === 'archived' && !group.callable)}
                      onSelect={() => (k.status === 'archived' ? action.mutate({ id: k.id, status: 'enabled' }) : setConfirm({ key: k }))}
                      className={k.status === 'archived' ? 'text-green-600' : 'text-orange-600'}
                    >
                      {k.status === 'archived' ? <IconCheck className='mr-2 h-4 w-4' /> : <IconArchive className='mr-2 h-4 w-4' />}
                      {t(k.status === 'archived' ? 'common.buttons.restore' : 'portal.archive')}
                    </DropdownMenuItem>
                    <DropdownMenuSeparator />
                    <DropdownMenuItem disabled={!group.member || action.isPending} onSelect={() => setConfirm({ key: k, rotate: true })}>
                      <IconRefresh className='mr-2 h-4 w-4' />
                      {t('portal.rotate')}
                    </DropdownMenuItem>
                  </RowActionsMenu>
                );
              },
            },
          ]}
        />
      ) : (
        <PortalEmpty title={t('portal.noKeys')} description={t('portal.noKeysHelp')} />
      )}
      {page.data && (
        <ServerSidePagination
          pageInfo={{ hasPreviousPage: cursors.length > 1, hasNextPage: !!page.data.keys.nextCursor }}
          pageSize={pageSize}
          dataLength={rows.length}
          totalCount={page.data.keys.totalCount}
          selectedRows={0}
          onFirstPage={resetPage}
          onPreviousPage={() => setCursors((c) => c.slice(0, -1))}
          onNextPage={() => {
            if (page.data.keys.nextCursor) setCursors((c) => [...c, page.data.keys.nextCursor]);
          }}
          onPageSizeChange={(size) => {
            setPageSize(size);
            resetPage();
          }}
        />
      )}
      {confirm && (
        <ConfirmDialog
          open
          onOpenChange={(open) => {
            if (!open) setConfirm(null);
          }}
          title={t(confirm.rotate ? 'portal.rotate' : 'portal.archive')}
          desc={t(confirm.rotate ? 'portal.rotateConfirm' : 'portal.archiveConfirm')}
          cancelBtnText={t('portal.cancel')}
          confirmText={t(confirm.rotate ? 'portal.rotate' : 'portal.archive')}
          destructive
          isLoading={action.isPending}
          handleConfirm={() =>
            action.mutate({ id: confirm.key.id, rotate: confirm.rotate, status: confirm.rotate ? undefined : 'archived' })
          }
        />
      )}
      {editor !== undefined && (
        <KeyEditor
          item={editor}
          onClose={() => {
            resetPage();
            setEditor(undefined);
          }}
        />
      )}
      {history && <KeyHistory item={history} onClose={() => setHistory(null)} />}
    </div>
  );
}
