import { useMutation, useQueryClient } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';
import { useTranslation } from 'react-i18next';
import { usePermissions } from '@/hooks/usePermissions';
import { Label } from '@/components/ui/label';
import { Switch } from '@/components/ui/switch';
import { useRegistrationStatus } from '@/features/auth/data/registration';

export function RegistrationSettings() {
  const { t } = useTranslation();
  const { hasSystemScope } = usePermissions();
  const status = useRegistrationStatus();
  const queryClient = useQueryClient();
  const update = useMutation({
    mutationFn: (enabled: boolean) =>
      graphqlRequest<{ setInvitationRegistration: boolean }>(
        'mutation Registration($enabled:Boolean!){setInvitationRegistration(enabled:$enabled)}',
        { enabled }
      ),
    onSuccess: (data) => {
      queryClient.setQueryData(['invitation-registration'], { enabled: data.setInvitationRegistration });
    },
  });
  return (
    <section className='space-y-2 rounded-lg border p-4'>
      <div className='flex items-center justify-between gap-4'>
        <Label htmlFor='invitation-registration'>{t('registration.title')}</Label>
        <Switch
          id='invitation-registration'
          checked={status.data?.enabled ?? false}
          onCheckedChange={(enabled) => update.mutate(enabled)}
          disabled={!hasSystemScope('write_settings') || !status.data || update.isPending}
        />
      </div>
      {(status.error || update.error) && (
        <p role='alert' className='text-destructive text-sm'>
          {(status.error || update.error)?.message}
        </p>
      )}
    </section>
  );
}
