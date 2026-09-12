'use client';

import { useEffect, useMemo, useState } from 'react';
import { Loader2, Save } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Switch } from '@/components/ui/switch';
import { TagsInput } from '@/components/ui/tags-input';
import { useSecuritySettings, useUpdateSecuritySettings } from '../data/system';

function normalizeEntries(entries: string[]) {
  const seen = new Set<string>();
  const result: string[] = [];

  for (const entry of entries) {
    const value = entry.trim();
    if (!value || seen.has(value)) {
      continue;
    }

    seen.add(value);
    result.push(value);
  }

  return result;
}

export function SecuritySettings() {
  const { t } = useTranslation();
  const { data: settings, isLoading } = useSecuritySettings();
  const updateSettings = useUpdateSecuritySettings();
  const [blockedIPs, setBlockedIPs] = useState<string[]>([]);
  const [showRequestLogIPBanIcon, setShowRequestLogIPBanIcon] = useState(true);
  const [cyberSessionBlockEnabled, setCyberSessionBlockEnabled] = useState(false);
  const [cyberSessionBlockTTLSeconds, setCyberSessionBlockTTLSeconds] = useState(3600);

  useEffect(() => {
    if (settings) {
      setBlockedIPs(settings.blockedIPs ?? []);
      setShowRequestLogIPBanIcon(settings.showRequestLogIPBanIcon ?? true);
      setCyberSessionBlockEnabled(settings.cyberSessionBlockEnabled ?? false);
      setCyberSessionBlockTTLSeconds(settings.cyberSessionBlockTTLSeconds ?? 3600);
    }
  }, [settings]);

  const normalizedBlockedIPs = useMemo(() => normalizeEntries(blockedIPs), [blockedIPs]);
  const hasChanges = settings
    ? normalizedBlockedIPs.join('\n') !== normalizeEntries(settings.blockedIPs ?? []).join('\n') ||
      showRequestLogIPBanIcon !== (settings.showRequestLogIPBanIcon ?? true) ||
      cyberSessionBlockEnabled !== (settings.cyberSessionBlockEnabled ?? false) ||
      cyberSessionBlockTTLSeconds !== (settings.cyberSessionBlockTTLSeconds ?? 3600)
    : false;

  const handleSave = async () => {
    await updateSettings.mutateAsync({
      blockedIPs: normalizedBlockedIPs,
      showRequestLogIPBanIcon,
      cyberSessionBlockEnabled,
      cyberSessionBlockTTLSeconds,
    });
  };

  if (isLoading) {
    return (
      <div className='flex h-32 items-center justify-center'>
        <Loader2 className='h-6 w-6 animate-spin' />
        <span className='text-muted-foreground ml-2'>{t('common.loading')}</span>
      </div>
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('system.security.title')}</CardTitle>
        <CardDescription>{t('system.security.description')}</CardDescription>
      </CardHeader>
      <CardContent className='space-y-6'>
        <div className='space-y-2'>
          <Label htmlFor='blocked-ips'>{t('system.security.blockedIPs.label')}</Label>
          <TagsInput
            id='blocked-ips'
            value={blockedIPs}
            onChange={setBlockedIPs}
            placeholder={t('system.security.blockedIPs.placeholder')}
            disabled={updateSettings.isPending}
          />
          <div className='text-muted-foreground text-sm'>{t('system.security.blockedIPs.description')}</div>
        </div>

        <div className='flex items-center justify-between gap-4 rounded-lg border p-4'>
          <div className='space-y-1'>
            <Label htmlFor='show-request-log-ip-ban-icon'>{t('system.security.showRequestLogIPBanIcon.label')}</Label>
            <div className='text-muted-foreground text-sm'>{t('system.security.showRequestLogIPBanIcon.description')}</div>
          </div>
          <Switch
            id='show-request-log-ip-ban-icon'
            checked={showRequestLogIPBanIcon}
            onCheckedChange={setShowRequestLogIPBanIcon}
            disabled={updateSettings.isPending}
          />
        </div>

        <div className='space-y-4 rounded-lg border p-4'>
          <div className='flex items-center justify-between gap-4'>
            <div className='space-y-1'>
              <Label htmlFor='cyber-session-auto-block'>{t('system.security.cyberSessionAutoBlock.label')}</Label>
              <div className='text-muted-foreground text-sm'>{t('system.security.cyberSessionAutoBlock.description')}</div>
            </div>
            <Switch
              id='cyber-session-auto-block'
              checked={cyberSessionBlockEnabled}
              onCheckedChange={setCyberSessionBlockEnabled}
              disabled={updateSettings.isPending}
            />
          </div>

          <div className='space-y-2'>
            <Label htmlFor='cyber-session-block-ttl'>{t('system.security.cyberSessionBlockTTL.label')}</Label>
            <div className='flex items-center gap-2'>
              <Input
                id='cyber-session-block-ttl'
                type='number'
                min={1}
                max={2147483647}
                step={1}
                value={cyberSessionBlockTTLSeconds}
                onChange={(event) => setCyberSessionBlockTTLSeconds(Number.parseInt(event.target.value, 10) || 0)}
                disabled={updateSettings.isPending || !cyberSessionBlockEnabled}
                className='max-w-40'
              />
              <span className='text-muted-foreground text-sm'>{t('system.security.cyberSessionBlockTTL.unit')}</span>
            </div>
            <div className='text-muted-foreground text-sm'>{t('system.security.cyberSessionBlockTTL.description')}</div>
          </div>
        </div>

        <div className='flex justify-end'>
          <Button
            onClick={handleSave}
            disabled={
              !hasChanges ||
              updateSettings.isPending ||
              cyberSessionBlockTTLSeconds <= 0 ||
              cyberSessionBlockTTLSeconds > 2147483647
            }
            className='min-w-[100px]'
          >
            {updateSettings.isPending ? (
              <>
                <Loader2 className='mr-2 h-4 w-4 animate-spin' />
                {t('system.buttons.saving')}
              </>
            ) : (
              <>
                <Save className='mr-2 h-4 w-4' />
                {t('system.buttons.save')}
              </>
            )}
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
