import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { Checkbox } from '@/components/ui/checkbox';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Label } from '@/components/ui/label';
import type { User } from '../data/schema';

export function UsersAccessModeDialog({
  currentRow,
  open,
  onOpenChange,
}: {
  currentRow: User;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const client = useQueryClient();
  const [mode, setMode] = useState(currentRow.accessMode);
  const [disable, setDisable] = useState(false);
  const save = useMutation({
    mutationFn: () =>
      graphqlRequest(
        'mutation AccessMode($id: ID!, $mode: UserAccessMode!, $disable: Boolean!) { setUserAccessMode(id: $id, mode: $mode, disableLegacyKeys: $disable) { id accessMode } }',
        { id: currentRow.id, mode, disable }
      ),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['users'] });
      onOpenChange(false);
    },
  });
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t('portal.accessMode')}</DialogTitle>
          <DialogDescription>{t('portal.modeHelp')}</DialogDescription>
        </DialogHeader>
        <div className='space-y-4'>
          <Label className='flex items-center justify-between'>
            {currentRow.email}
            <select className='bg-background rounded-md border p-2' value={mode} onChange={(e) => setMode(e.target.value as typeof mode)}>
              <option value='legacy'>{t('portal.legacyMode')}</option>
              <option value='portal'>{t('portal.portalMode')}</option>
            </select>
          </Label>
          {mode === 'portal' && (
            <Label className='flex items-start gap-2 leading-relaxed'>
              <Checkbox checked={disable} onCheckedChange={(v) => setDisable(v === true)} />
              {t('portal.disableOldKeys')}
            </Label>
          )}
          {save.error && (
            <p role='alert' className='text-destructive text-sm'>
              {save.error.message}
            </p>
          )}
          <Button disabled={save.isPending} onClick={() => save.mutate()}>
            {t('portal.save')}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
