import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import AuthLayout from '../auth-layout';
import { useRegistrationStatus } from '../data/registration';
import { SignUpForm } from './components/sign-up-form';

export default function SignUp() {
  const { t } = useTranslation();
  const status = useRegistrationStatus();
  return (
    <AuthLayout>
      <Card className='w-full max-w-md gap-4'>
        <CardHeader>
          <CardTitle className='text-lg tracking-tight'>{t('registration.signUp')}</CardTitle>
        </CardHeader>
        <CardContent className='space-y-5'>
          {status.isLoading ? (
            <Skeleton className='h-48 w-full' />
          ) : status.error ? (
            <p role='alert' className='text-destructive text-sm'>
              {status.error.message}
            </p>
          ) : status.data?.enabled ? (
            <SignUpForm />
          ) : (
            <p className='text-muted-foreground text-sm'>{t('registration.closed')}</p>
          )}
          <Link to='/sign-in' className='text-primary block text-center text-sm hover:underline'>
            {t('registration.signIn')}
          </Link>
        </CardContent>
      </Card>
    </AuthLayout>
  );
}
