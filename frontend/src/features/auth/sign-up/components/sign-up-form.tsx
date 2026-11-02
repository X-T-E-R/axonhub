import type { HTMLAttributes } from 'react';
import { z } from 'zod';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { useMutation } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { authApi } from '@/lib/api-client';
import { cn } from '@/lib/utils';
import { passwordSchema } from '@/lib/validation';
import { Button } from '@/components/ui/button';
import { Form, FormControl, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { PasswordInput } from '@/components/password-input';
import { useSignIn } from '../../data/auth';

export function SignUpForm({ className, ...props }: HTMLAttributes<HTMLFormElement>) {
  const { t } = useTranslation();
  const schema = z
    .object({
      firstName: z.string().trim().min(1, t('users.validation.firstNameRequired')).max(128),
      lastName: z.string().trim().min(1, t('users.validation.lastNameRequired')).max(128),
      email: z.email().max(254),
      password: passwordSchema(t).refine((value) => new TextEncoder().encode(value).length <= 72, t('registration.passwordTooLong')),
      confirmPassword: z.string(),
      inviteCode: z.string().trim().min(1),
    })
    .refine((data) => data.password === data.confirmPassword, {
      message: t('users.validation.passwordsNotMatch'),
      path: ['confirmPassword'],
    });
  const form = useForm<z.infer<typeof schema>>({
    resolver: zodResolver(schema),
    defaultValues: { firstName: '', lastName: '', email: '', password: '', confirmPassword: '', inviteCode: '' },
  });
  const signIn = useSignIn();
  const register = useMutation({
    mutationFn: (data: z.infer<typeof schema>) => authApi.signUp(data),
    onSuccess: async (_, data) => {
      await signIn.mutateAsync({ email: data.email, password: data.password });
    },
  });
  return (
    <Form {...form}>
      <form onSubmit={form.handleSubmit((data) => register.mutate(data))} className={cn('grid gap-4', className)} {...props}>
        <div className='grid grid-cols-2 gap-3'>
          {(['firstName', 'lastName'] as const).map((name) => (
            <FormField
              key={name}
              control={form.control}
              name={name}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t(`portal.${name}`)}</FormLabel>
                  <FormControl>
                    <Input autoComplete={name === 'firstName' ? 'given-name' : 'family-name'} {...field} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
          ))}
        </div>
        <FormField
          control={form.control}
          name='email'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('auth.signIn.form.email.label')}</FormLabel>
              <FormControl>
                <Input type='email' autoComplete='email' {...field} />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
        <FormField
          control={form.control}
          name='password'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('auth.signIn.form.password.label')}</FormLabel>
              <FormControl>
                <PasswordInput autoComplete='new-password' {...field} />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
        <FormField
          control={form.control}
          name='confirmPassword'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('users.form.confirmPassword')}</FormLabel>
              <FormControl>
                <PasswordInput autoComplete='new-password' {...field} />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
        <FormField
          control={form.control}
          name='inviteCode'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('registration.inviteCode')}</FormLabel>
              <FormControl>
                <Input autoComplete='off' spellCheck={false} {...field} />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
        {(register.error || signIn.error) && (
          <p role='alert' className='text-destructive text-sm'>
            {(register.error || signIn.error)?.message}
          </p>
        )}
        <Button type='submit' disabled={register.isPending || signIn.isPending}>
          {t('registration.signUp')}
        </Button>
      </form>
    </Form>
  );
}
