import { HTMLAttributes } from 'react';
import { Link } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';
import { zodResolver } from '@hookform/resolvers/zod';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { Button } from '@/components/ui/button';
import { Form, FormControl, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { PasswordInput } from '@/components/password-input';
import { useSignUp } from '@/features/auth/data/auth';
import { authApi } from '@/lib/api-client';
import { cn } from '@/lib/utils';
import { passwordSchema } from '@/lib/validation';

type SignUpFormProps = HTMLAttributes<HTMLFormElement>;

const fieldClassName =
  'border-slate-300 !bg-white text-slate-800 placeholder:text-slate-400 focus:border-slate-500 focus:ring-slate-500/20';

const createFormSchema = (t: (key: string) => string) =>
  z
    .object({
      email: z.string().min(1, { message: t('auth.signIn.validation.emailRequired') }).email({ message: t('profile.form.validation.emailInvalid') }),
      firstName: z.string().optional(),
      lastName: z.string().optional(),
      inviteCode: z.string().optional(),
      password: passwordSchema(t),
      confirmPassword: z.string(),
    })
    .refine((data) => data.password === data.confirmPassword, {
      message: t('users.validation.passwordsNotMatch'),
      path: ['confirmPassword'],
    });

export function SignUpForm({ className, ...props }: SignUpFormProps) {
  const { t } = useTranslation();
  const formSchema = createFormSchema(t);
  const signUp = useSignUp();
  const {
    data: policy,
    error: policyError,
    isLoading: policyLoading,
  } = useQuery({
    queryKey: ['signup-policy'],
    queryFn: authApi.signUpPolicy,
    retry: 1,
  });

  const form = useForm<z.infer<typeof formSchema>>({
    resolver: zodResolver(formSchema),
    defaultValues: {
      email: '',
      firstName: '',
      lastName: '',
      inviteCode: '',
      password: '',
      confirmPassword: '',
    },
  });

  function onSubmit(data: z.infer<typeof formSchema>) {
    if (policy?.inviteCodeRequired && !data.inviteCode?.trim()) {
      form.setError('inviteCode', {
        message: t('auth.signUp.validation.inviteCodeRequired'),
      });
      return;
    }

    signUp.mutate({
      email: data.email.trim(),
      password: data.password,
      firstName: data.firstName?.trim() || undefined,
      lastName: data.lastName?.trim() || undefined,
      inviteCode: data.inviteCode?.trim() || undefined,
    });
  }

  const passwordSignupDisabled = policy?.passwordSignupAllowed === false;
  const isDisabled = policyLoading || signUp.isPending || policy?.enabled === false || passwordSignupDisabled;
  const disabledReason = policyLoading
    ? t('auth.signUp.status.checkingPolicy')
    : policy?.enabled === false
      ? t('auth.signUp.status.registrationClosed')
      : passwordSignupDisabled
        ? t('auth.signUp.status.passwordDisabled')
        : undefined;

  return (
    <Form {...form}>
      <form onSubmit={form.handleSubmit(onSubmit)} className={cn('grid gap-5', className)} {...props}>
        {policyError && (
          <div className='rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm leading-relaxed text-amber-800'>
            {t('auth.signUp.alerts.policyLoadFailed')}
          </div>
        )}

        {policy?.enabled === false && (
          <div className='rounded-xl border border-slate-200 bg-slate-50 px-4 py-3 text-sm leading-relaxed text-slate-600'>
            {t('auth.signUp.alerts.registrationClosed')}{' '}
            <Link to='/sign-in' className='font-medium text-slate-900 underline underline-offset-4'>
              {t('auth.signUp.links.signIn')}
            </Link>
            .
          </div>
        )}

        {passwordSignupDisabled && (
          <div className='rounded-xl border border-slate-200 bg-slate-50 px-4 py-3 text-sm leading-relaxed text-slate-600'>
            {t('auth.signUp.alerts.passwordDisabled')}{' '}
            <Link to='/sign-in' className='font-medium text-slate-900 underline underline-offset-4'>
              {t('auth.signUp.links.signIn')}
            </Link>
            .
          </div>
        )}

        <FormField
          control={form.control}
          name='email'
          render={({ field }) => (
            <FormItem>
              <FormLabel className='text-sm font-medium text-slate-700'>{t('auth.signUp.form.email.label')}</FormLabel>
              <FormControl>
                <Input
                  placeholder={t('auth.signUp.form.email.placeholder')}
                  className={fieldClassName}
                  autoComplete='email'
                  data-testid='sign-up-email'
                  {...field}
                />
              </FormControl>
              <FormMessage className='text-red-600' />
            </FormItem>
          )}
        />

        <div className='grid gap-3 sm:grid-cols-2'>
          <FormField
            control={form.control}
            name='firstName'
            render={({ field }) => (
              <FormItem>
                <FormLabel className='text-sm font-medium text-slate-700'>{t('auth.signUp.form.firstName.label')}</FormLabel>
                <FormControl>
                  <Input
                    placeholder={t('auth.signUp.form.firstName.placeholder')}
                    className={fieldClassName}
                    autoComplete='given-name'
                    {...field}
                  />
                </FormControl>
                <FormMessage className='text-red-600' />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='lastName'
            render={({ field }) => (
              <FormItem>
                <FormLabel className='text-sm font-medium text-slate-700'>{t('auth.signUp.form.lastName.label')}</FormLabel>
                <FormControl>
                  <Input
                    placeholder={t('auth.signUp.form.lastName.placeholder')}
                    className={fieldClassName}
                    autoComplete='family-name'
                    {...field}
                  />
                </FormControl>
                <FormMessage className='text-red-600' />
              </FormItem>
            )}
          />
        </div>

        {policy?.inviteCodeRequired && (
          <FormField
            control={form.control}
            name='inviteCode'
            render={({ field }) => (
              <FormItem>
                <FormLabel className='text-sm font-medium text-slate-700'>{t('auth.signUp.form.inviteCode.label')}</FormLabel>
                <FormControl>
                  <Input
                    placeholder={t('auth.signUp.form.inviteCode.placeholder')}
                    className={fieldClassName}
                    autoComplete='one-time-code'
                    {...field}
                  />
                </FormControl>
                <FormMessage className='text-red-600' />
              </FormItem>
            )}
          />
        )}

        <FormField
          control={form.control}
          name='password'
          render={({ field }) => (
            <FormItem>
              <FormLabel className='text-sm font-medium text-slate-700'>{t('auth.signUp.form.password.label')}</FormLabel>
              <FormControl>
                <PasswordInput
                  placeholder={t('auth.signUp.form.password.placeholder')}
                  className={fieldClassName}
                  autoComplete='new-password'
                  data-testid='sign-up-password'
                  {...field}
                />
              </FormControl>
              <FormMessage className='text-red-600' />
            </FormItem>
          )}
        />

        <FormField
          control={form.control}
          name='confirmPassword'
          render={({ field }) => (
            <FormItem>
              <FormLabel className='text-sm font-medium text-slate-700'>{t('auth.signUp.form.confirmPassword.label')}</FormLabel>
              <FormControl>
                <PasswordInput
                  placeholder={t('auth.signUp.form.confirmPassword.placeholder')}
                  className={fieldClassName}
                  autoComplete='new-password'
                  data-testid='sign-up-confirm-password'
                  {...field}
                />
              </FormControl>
              <FormMessage className='text-red-600' />
            </FormItem>
          )}
        />

        <Button
          type='submit'
          disabled={isDisabled}
          data-testid='sign-up-submit'
          className='mt-1 h-11 w-full rounded-lg bg-slate-800 font-medium text-white shadow-lg shadow-slate-900/20 transition-all duration-200 hover:bg-slate-900 hover:shadow-xl hover:shadow-slate-900/30 disabled:opacity-60'
        >
          {signUp.isPending ? t('auth.signUp.form.creatingAccount') : t('auth.signUp.form.createAccount')}
        </Button>

        {disabledReason && <p className='text-center text-xs text-slate-500'>{disabledReason}</p>}
      </form>
    </Form>
  );
}
