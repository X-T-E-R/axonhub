import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { useAuthStore } from '@/stores/authStore';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { useOIDCProviders } from '@/features/auth/data/auth';
import OidcManagement from '@/features/settings/security/oidc-management';
import { portalRequest } from './data';

export default function PortalProfile() {
  const { t } = useTranslation();
  const user = useAuthStore((s) => s.auth.user);
  const client = useQueryClient();
  const { data: providers = [] } = useOIDCProviders();
  const [firstName, setFirstName] = useState(user?.firstName || '');
  const [lastName, setLastName] = useState(user?.lastName || '');
  const [preferLanguage, setLanguage] = useState(user?.preferLanguage || 'en');
  const [oldPassword, setOldPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const profile = useMutation({
    mutationFn: () =>
      portalRequest(
        'mutation Profile($firstName: String, $lastName: String, $preferLanguage: String) { updateProfile(firstName: $firstName, lastName: $lastName, preferLanguage: $preferLanguage) { id } }',
        { firstName, lastName, preferLanguage }
      ),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['me'] });
    },
  });
  const password = useMutation({
    mutationFn: () =>
      portalRequest(
        'mutation Password($oldPassword: String, $newPassword: String!) { updatePassword(oldPassword: $oldPassword, newPassword: $newPassword) }',
        { oldPassword: oldPassword || null, newPassword }
      ),
    onSuccess: () => {
      setOldPassword('');
      setNewPassword('');
    },
  });
  return (
    <div className='max-w-xl space-y-8'>
      <div>
        <h1 className='text-2xl font-semibold'>{t('portal.profile')}</h1>
        <p className='text-muted-foreground mt-2 text-sm'>{user?.email}</p>
      </div>
      <form
        className='space-y-4'
        onSubmit={(e) => {
          e.preventDefault();
          profile.mutate();
        }}
      >
        <div className='space-y-2'>
          <Label htmlFor='first-name'>{t('portal.firstName')}</Label>
          <Input id='first-name' value={firstName} onChange={(e) => setFirstName(e.target.value)} />
        </div>
        <div className='space-y-2'>
          <Label htmlFor='last-name'>{t('portal.lastName')}</Label>
          <Input id='last-name' value={lastName} onChange={(e) => setLastName(e.target.value)} />
        </div>
        <div className='space-y-2'>
          <Label htmlFor='language'>{t('portal.language')}</Label>
          <select
            id='language'
            value={preferLanguage}
            onChange={(e) => setLanguage(e.target.value)}
            className='bg-background w-full rounded-md border p-2'
          >
            <option value='en'>English</option>
            <option value='zh'>简体中文</option>
          </select>
        </div>
        {profile.error && (
          <p role='alert' className='text-destructive text-sm'>
            {profile.error.message}
          </p>
        )}
        {profile.isSuccess && (
          <p role='status' className='text-sm'>
            {t('portal.saved')}
          </p>
        )}
        <Button type='submit' disabled={profile.isPending}>
          {t('portal.save')}
        </Button>
      </form>
      <form
        className='space-y-4 border-t pt-6'
        onSubmit={(e) => {
          e.preventDefault();
          password.mutate();
        }}
      >
        <h2 className='text-lg font-semibold'>{t('portal.password')}</h2>
        {user?.hasPassword && (
          <div className='space-y-2'>
            <Label htmlFor='old-password'>{t('portal.oldPassword')}</Label>
            <Input
              id='old-password'
              type='password'
              autoComplete='current-password'
              required
              value={oldPassword}
              onChange={(e) => setOldPassword(e.target.value)}
            />
          </div>
        )}
        <div className='space-y-2'>
          <Label htmlFor='new-password'>{t('portal.newPassword')}</Label>
          <Input
            id='new-password'
            type='password'
            autoComplete='new-password'
            required
            minLength={8}
            maxLength={72}
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
          />
        </div>
        {password.error && (
          <p role='alert' className='text-destructive text-sm'>
            {password.error.message}
          </p>
        )}
        {password.isSuccess && <p role='status'>{t('portal.saved')}</p>}
        <Button type='submit' disabled={password.isPending}>
          {t('portal.password')}
        </Button>
      </form>
      {providers.length > 0 && <OidcManagement providers={providers} portal />}
    </div>
  );
}
