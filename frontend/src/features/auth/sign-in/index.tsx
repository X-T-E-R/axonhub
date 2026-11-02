import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import AuthLayout from '../auth-layout';
import TwoColumnAuth from '../components/two-column-auth';
import { useRegistrationStatus } from '../data/registration';
import AnimatedLineBackground from './components/animated-line-background';
import { UserAuthForm } from './components/user-auth-form';
import './login-styles.css';

export default function SignIn() {
  const { t } = useTranslation();
  const registration = useRegistrationStatus();

  return (
    <AuthLayout>
      <div data-testid='sign-in-animation-layer'>
        <AnimatedLineBackground key='optimized-layout' />
      </div>
      <TwoColumnAuth
        title={t('auth.signIn.title')}
        description={t('auth.signIn.subtitle')}
        rightFooter={<p className='text-xs leading-relaxed text-slate-500 sm:text-sm'>{t('auth.signIn.footer.agreement')}</p>}
      >
        <UserAuthForm />
        {registration.data?.enabled && (
          <Link to='/sign-up' className='mt-6 block text-center text-sm font-medium text-slate-600 hover:underline'>
            {t('registration.signUp')}
          </Link>
        )}
      </TwoColumnAuth>
    </AuthLayout>
  );
}
