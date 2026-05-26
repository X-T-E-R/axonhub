import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import AuthLayout from '../auth-layout';
import TwoColumnAuth from '../components/two-column-auth';
import AnimatedLineBackground from '../sign-in/components/animated-line-background';
import { SignUpForm } from './components/sign-up-form';
import '../sign-in/login-styles.css';

export default function SignUp() {
  const { t } = useTranslation();

  return (
    <AuthLayout>
      <div data-testid='sign-up-animation-layer'>
        <AnimatedLineBackground key='signup-layout' />
      </div>
      <TwoColumnAuth
        title={t('auth.signUp.title')}
        description={t('auth.signUp.subtitle')}
        rightMaxWidthClassName='max-w-lg'
        rightFooter={
          <div className='space-y-3 text-center'>
            <p className='text-sm text-slate-600'>
              {t('auth.signUp.footer.hasAccount')}{' '}
              <Link to='/sign-in' className='font-semibold text-slate-950 underline underline-offset-4 hover:text-slate-700'>
                {t('auth.signUp.links.signIn')}
              </Link>
            </p>
            <p className='text-xs leading-relaxed text-slate-500 sm:text-sm'>{t('auth.signUp.footer.agreement')}</p>
          </div>
        }
      >
        <SignUpForm />
      </TwoColumnAuth>
    </AuthLayout>
  );
}
