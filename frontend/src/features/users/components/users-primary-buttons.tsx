import { IconUserPlus } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { usePermissions } from '@/hooks/usePermissions';
import { Button } from '@/components/ui/button';
import { PermissionGuard } from '@/components/permission-guard';
import { useUsers } from '../context/users-context';

export function UsersPrimaryButtons() {
  const { t } = useTranslation();
  const { setOpen } = useUsers();
  const { hasSystemScope } = usePermissions();
  return (
    <div className='flex gap-2'>
      {hasSystemScope('write_users') && (
        <Button variant='outline' onClick={() => setOpen('invite')}>
          {t('registration.invites')}
        </Button>
      )}
      <PermissionGuard requiredScope='write_users'>
        <Button className='space-x-1' onClick={() => setOpen('add')}>
          <span>{t('users.addUser')}</span> <IconUserPlus size={18} />
        </Button>
      </PermissionGuard>
    </div>
  );
}
