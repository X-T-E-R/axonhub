import { useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';
import { Copy } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Skeleton } from '@/components/ui/skeleton';
import { PortalTable } from '@/features/portal/table';
import { RegistrationSettings } from '@/features/system/components/registration-settings';
import {
  CREATE_INVITE,
  INVITES_QUERY,
  REVOKE_INVITE,
  type RegistrationGroup,
  type RegistrationInvite,
  type RegistrationInviteInput,
} from '../data/registration';

export function UsersInviteDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const { t } = useTranslation();
  // Mount the working form only while open: generated plaintext never remains in
  // a hidden dialog or React Query's mutation cache after closing it.
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='max-h-[90dvh] overflow-y-auto sm:max-w-4xl' aria-describedby={undefined}>
        <DialogHeader>
          <DialogTitle>{t('registration.invites')}</DialogTitle>
        </DialogHeader>
        {open && <InviteManager />}
      </DialogContent>
    </Dialog>
  );
}
function InviteManager() {
  const { t, i18n } = useTranslation();
  const [projectID, setProjectID] = useState('');
  const [maxUses, setMaxUses] = useState(1);
  const [expiresAt, setExpiresAt] = useState('');
  const [code, setCode] = useState('');
  const query = useQuery({
    queryKey: ['registration-invites'],
    queryFn: () => graphqlRequest<{ registrationGroups: RegistrationGroup[]; registrationInvites: RegistrationInvite[] }>(INVITES_QUERY),
    staleTime: 0,
  });
  const create = useMutation({
    mutationFn: async () => {
      const input: RegistrationInviteInput = {
        projectID: Number(projectID),
        maxUses,
        expiresAt: expiresAt ? new Date(expiresAt).toISOString() : null,
      };
      // Keep only the one-time component state, never return plaintext into the mutation cache.
      const result = await graphqlRequest<{ createRegistrationInvite: { code: string } }>(CREATE_INVITE, { input });
      setCode(result.createRegistrationInvite.code);
    },
    onSuccess: () => {
      void query.refetch();
    },
  });
  const revoke = useMutation({
    mutationFn: (id: number) => graphqlRequest(REVOKE_INVITE, { id }),
    onSuccess: () => {
      void query.refetch();
    },
  });
  return (
    <div className='space-y-5'>
      <RegistrationSettings />
      <form
        className='grid items-end gap-3 sm:grid-cols-2 lg:grid-cols-[1fr_100px_1fr_auto]'
        onSubmit={(event) => {
          event.preventDefault();
          setCode('');
          create.mutate();
        }}
      >
        <div className='space-y-2'>
          <Label htmlFor='invite-group'>{t('registration.group')}</Label>
          <select
            id='invite-group'
            required
            className='bg-background h-9 w-full rounded-md border px-2 text-sm'
            value={projectID}
            onChange={(event) => setProjectID(event.target.value)}
          >
            <option value='' disabled>
              {t('registration.selectGroup')}
            </option>
            {query.data?.registrationGroups.map((group) => (
              <option key={group.id} value={group.id}>
                {group.name}
              </option>
            ))}
          </select>
        </div>
        <div className='space-y-2'>
          <Label htmlFor='invite-uses'>{t('registration.maxUses')}</Label>
          <Input
            id='invite-uses'
            type='number'
            min={1}
            max={10000}
            required
            value={maxUses}
            onChange={(event) => setMaxUses(Number(event.target.value))}
          />
        </div>
        <div className='space-y-2'>
          <Label htmlFor='invite-expiry'>{t('registration.expiresAt')}</Label>
          <Input id='invite-expiry' type='datetime-local' value={expiresAt} onChange={(event) => setExpiresAt(event.target.value)} />
        </div>
        <Button type='submit' disabled={!projectID || create.isPending}>
          {t('registration.generate')}
        </Button>
      </form>
      {code && (
        <div className='space-y-2 rounded-lg border p-4'>
          <Label htmlFor='generated-invite'>{t('registration.copyOnce')}</Label>
          <div className='flex items-center gap-2'>
            <Input id='generated-invite' readOnly value={code} className='font-mono' />
            <Button
              variant='outline'
              size='icon'
              aria-label={t('portal.copyCode')}
              onClick={async () => {
                try {
                  await navigator.clipboard.writeText(code);
                  toast.success(t('portal.copied'));
                } catch {
                  toast.error(t('portal.copyFailed'));
                }
              }}
            >
              <Copy className='size-4' />
            </Button>
          </div>
        </div>
      )}
      {(query.error || create.error || revoke.error) && (
        <p role='alert' className='text-destructive text-sm'>
          {(query.error || create.error || revoke.error)?.message}
        </p>
      )}
      {query.isLoading ? (
        <Skeleton className='h-48 w-full' />
      ) : (
        <PortalTable
          data={query.data?.registrationInvites || []}
          columns={[
            {
              accessorKey: 'id',
              header: t('common.columns.id'),
              cell: ({ row }) => <span className='font-mono text-xs'>#{row.original.id}</span>,
            },
            { accessorKey: 'groupName', header: t('registration.group') },
            { id: 'status', header: t('portal.status'), cell: ({ row }) => t(`registration.${row.original.status}`) },
            { id: 'usage', header: t('registration.usage'), cell: ({ row }) => `${row.original.usedUses} / ${row.original.maxUses}` },
            {
              id: 'expiry',
              header: t('registration.expiresAt'),
              cell: ({ row }) =>
                row.original.expiresAt ? new Date(row.original.expiresAt).toLocaleString(i18n.language) : t('registration.noExpiry'),
            },
            {
              id: 'actions',
              header: t('portal.actions'),
              cell: ({ row }) => (
                <Button
                  variant='ghost'
                  size='sm'
                  disabled={row.original.status === 'revoked' || revoke.isPending}
                  onClick={() => revoke.mutate(row.original.id)}
                >
                  {t('registration.revoke')}
                </Button>
              ),
            },
          ]}
        />
      )}
    </div>
  );
}
