import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import AuthLayout from '../auth-layout';
import TwoColumnAuth from '../components/two-column-auth';
import AnimatedLineBackground from './components/animated-line-background';
import { UserAuthForm } from './components/user-auth-form';
import './login-styles.css';

export default function SignIn() {
  const { t } = useTranslation();

  return (
    <AuthLayout>
      <div data-testid='sign-in-animation-layer'>
        <AnimatedLineBackground key='optimized-layout' />
      </div>
      <TwoColumnAuth
        title={t('auth.signIn.title')}
        description={t('auth.signIn.subtitle')}
        rightFooter={
          <div className='space-y-3 text-center'>
            <p className='text-sm text-slate-600'>
              {t('auth.signIn.footer.noAccount')}{' '}
              <Link to='/sign-up' className='font-semibold text-slate-950 underline underline-offset-4 hover:text-slate-700'>
                {t('auth.signIn.links.createAccount')}
              </Link>
            </p>
            <p className='text-xs leading-relaxed text-slate-500 sm:text-sm'>{t('auth.signIn.footer.agreement')}</p>
          </div>
        }
      >
        <UserAuthForm />
      </TwoColumnAuth>
    </AuthLayout>
  );
}
