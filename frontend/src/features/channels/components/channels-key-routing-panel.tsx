'use client';

import type { UseFormReturn } from 'react-hook-form';
import { IconDatabase, IconRoute } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { Alert, AlertDescription } from '@/components/ui/alert';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import type { ChannelKeySelectionStrategy } from '../data/schema';

export const DEFAULT_LIKELY_AFFINITY_TTL_MINUTES = 30;
export const DEFAULT_EXACT_AFFINITY_TTL_MINUTES = 1440;

export type ChannelKeyRoutingStrategyValue = 'inherit' | ChannelKeySelectionStrategy;

export type KeyRoutingFormValues = {
  strategy: ChannelKeyRoutingStrategyValue;
  likelyAffinityTTLMinutes: number;
  exactAffinityTTLMinutes: number;
};

const STRATEGIES: ChannelKeyRoutingStrategyValue[] = ['inherit', 'trace_sticky', 'cache_affinity', 'random', 'round_robin'];

interface KeyRoutingPanelProps<TFormValues extends KeyRoutingFormValues> {
  form: UseFormReturn<TFormValues, unknown, TFormValues>;
  globalStrategy?: ChannelKeySelectionStrategy | null;
}

export function KeyRoutingPanel<TFormValues extends KeyRoutingFormValues>({ form, globalStrategy }: KeyRoutingPanelProps<TFormValues>) {
  const { t } = useTranslation();
  const selectedStrategy = form.watch('strategy' as never) as ChannelKeyRoutingStrategyValue;
  const effectiveStrategy = selectedStrategy === 'inherit' ? (globalStrategy ?? 'trace_sticky') : selectedStrategy;

  return (
    <Card>
      <CardHeader>
        <CardTitle className='flex items-center gap-2'>
          <IconRoute className='h-5 w-5' />
          {t('channels.dialogs.keys.routing.title')}
        </CardTitle>
        <CardDescription>{t('channels.dialogs.keys.routing.description')}</CardDescription>
      </CardHeader>
      <CardContent className='space-y-4'>
        <FormField
          control={form.control}
          name={'strategy' as never}
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('channels.dialogs.keyRouting.fields.strategy.label')}</FormLabel>
              <Select value={field.value} onValueChange={field.onChange}>
                <FormControl>
                  <SelectTrigger>
                    <SelectValue placeholder={t('channels.dialogs.keyRouting.fields.strategy.placeholder')} />
                  </SelectTrigger>
                </FormControl>
                <SelectContent>
                  {STRATEGIES.map((strategy) => (
                    <SelectItem key={strategy} value={strategy}>
                      {strategy === 'inherit'
                        ? t('channels.dialogs.keyRouting.strategies.inherit.label', {
                            strategy: t(`channels.dialogs.keyRouting.strategies.${globalStrategy ?? 'trace_sticky'}.label`),
                          })
                        : t(`channels.dialogs.keyRouting.strategies.${strategy}.label`)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <FormDescription>
                {selectedStrategy === 'inherit'
                  ? t('channels.dialogs.keyRouting.fields.strategy.inheritDescription', {
                      strategy: t(`channels.dialogs.keyRouting.strategies.${globalStrategy ?? 'trace_sticky'}.label`),
                    })
                  : t('channels.dialogs.keyRouting.fields.strategy.description')}
              </FormDescription>
              <FormMessage />
            </FormItem>
          )}
        />

        {effectiveStrategy === 'cache_affinity' && selectedStrategy !== 'inherit' ? (
          <div className='grid gap-4 md:grid-cols-2'>
            <FormField
              control={form.control}
              name={'likelyAffinityTTLMinutes' as never}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('channels.dialogs.keyRouting.fields.likelyAffinityTTLMinutes.label')}</FormLabel>
                  <FormControl>
                    <Input type='number' min={1} max={1440} placeholder={String(DEFAULT_LIKELY_AFFINITY_TTL_MINUTES)} {...field} />
                  </FormControl>
                  <FormDescription>{t('channels.dialogs.keyRouting.fields.likelyAffinityTTLMinutes.description')}</FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name={'exactAffinityTTLMinutes' as never}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('channels.dialogs.keyRouting.fields.exactAffinityTTLMinutes.label')}</FormLabel>
                  <FormControl>
                    <Input type='number' min={1} max={10080} placeholder={String(DEFAULT_EXACT_AFFINITY_TTL_MINUTES)} {...field} />
                  </FormControl>
                  <FormDescription>{t('channels.dialogs.keyRouting.fields.exactAffinityTTLMinutes.description')}</FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />
          </div>
        ) : null}

        <Alert>
          <IconDatabase className='h-4 w-4' />
          <AlertDescription>
            <div className='font-medium'>
              {selectedStrategy === 'inherit'
                ? t('channels.dialogs.keyRouting.strategies.inherit.effective', {
                    strategy: t(`channels.dialogs.keyRouting.strategies.${effectiveStrategy}.label`),
                  })
                : t(`channels.dialogs.keyRouting.strategies.${effectiveStrategy}.label`)}
            </div>
            <div className='mt-1 text-sm'>
              {selectedStrategy === 'inherit'
                ? t('channels.dialogs.keyRouting.strategies.inherit.description')
                : t(`channels.dialogs.keyRouting.strategies.${effectiveStrategy}.description`)}
            </div>
          </AlertDescription>
        </Alert>
      </CardContent>
    </Card>
  );
}
