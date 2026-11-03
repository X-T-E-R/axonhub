import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { extractNumberID } from '@/lib/utils';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { useCreateChannel, useUpdateChannel } from '../data/channels';
import { Channel, CreateChannelInput } from '../data/schema';
import { mergeChannelSettingsForUpdate } from '../utils/merge';

const presets = {
  exa: { baseURL: 'https://api.exa.ai', resource: 'exa-search', authType: 'header' as const, authHeader: 'x-api-key' },
  tavily: { baseURL: 'https://api.tavily.com', resource: 'tavily-search', authType: 'bearer' as const, authHeader: '' },
  openai: { baseURL: 'https://api.openai.com/v1/images', resource: 'openai-images', authType: 'bearer' as const, authHeader: '' },
};

type Props = { open: boolean; onOpenChange: (open: boolean) => void; currentRow?: Channel; duplicate?: boolean };

export function ChannelsRawHTTPDialog({ open, onOpenChange, currentRow, duplicate = false }: Props) {
  const { t } = useTranslation();
  const create = useCreateChannel();
  const update = useUpdateChannel();
  const initial = currentRow?.settings?.rawHTTP;
  const [name, setName] = useState(duplicate ? `${currentRow?.name ?? ''} (copy)` : (currentRow?.name ?? ''));
  const [baseURL, setBaseURL] = useState(currentRow?.baseURL ?? '');
  const [resource, setResource] = useState(initial?.resource ?? '');
  const [authType, setAuthType] = useState<'bearer' | 'header'>(initial?.authType ?? 'bearer');
  const [authHeader, setAuthHeader] = useState(initial?.authHeader ?? '');
  const [apiKey, setAPIKey] = useState('');
  const editing = !!currentRow && !duplicate;
  const pending = create.isPending || update.isPending;

  async function save(e: React.FormEvent) {
    e.preventDefault();
    const alias = resource.trim();
    const settings = mergeChannelSettingsForUpdate(currentRow?.settings, {
      rawHTTP: { enabled: true, resource: alias, authType, authHeader: authType === 'header' ? authHeader.trim() : '' },
    });
    const input: CreateChannelInput = {
      type: 'openai',
      name: name.trim(),
      baseURL: baseURL.trim(),
      supportedModels: [alias],
      manualModels: [alias],
      defaultTestModel: alias,
      autoSyncSupportedModels: false,
      autoSyncModelPattern: '',
      tags: currentRow?.tags ?? [],
      endpoints: [],
      settings,
      credentials: { apiKeys: apiKey ? [apiKey.trim()] : [] },
    };
    try {
      if (editing) {
        const { credentials, ...rest } = input;
        await update.mutateAsync({ id: currentRow.id, input: { ...rest, ...(apiKey.trim() ? { credentials } : {}) } });
      } else {
        await create.mutateAsync(input);
      }
      setAPIKey('');
      onOpenChange(false);
    } catch {
      /* The shared mutation hook displays the API error. */
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='flex max-h-[90vh] flex-col sm:max-w-xl'>
        <DialogHeader>
          <DialogTitle>{t('channels.rawHTTP.title')}</DialogTitle>
          <DialogDescription>{t('channels.rawHTTP.description')}</DialogDescription>
        </DialogHeader>
        <form id='raw-http-form' onSubmit={save} className='min-h-0 space-y-4 overflow-y-auto px-0.5'>
          {!editing && (
            <div className='space-y-2'>
              <Label>{t('channels.rawHTTP.preset')}</Label>
              <Select
                defaultValue='custom'
                onValueChange={(value) => {
                  const preset = presets[value as keyof typeof presets];
                  if (!preset) return;
                  setBaseURL(preset.baseURL);
                  setResource(preset.resource);
                  setAuthType(preset.authType);
                  setAuthHeader(preset.authHeader);
                  setAPIKey('');
                  if (!name) setName(value === 'openai' ? 'OpenAI Images' : value === 'exa' ? 'Exa' : 'Tavily');
                }}
              >
                <SelectTrigger data-testid='raw-preset'>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value='custom'>{t('channels.rawHTTP.custom')}</SelectItem>
                  <SelectItem value='exa'>Exa</SelectItem>
                  <SelectItem value='tavily'>Tavily</SelectItem>
                  <SelectItem value='openai'>OpenAI Images</SelectItem>
                </SelectContent>
              </Select>
            </div>
          )}
          <div className='space-y-2'>
            <Label htmlFor='raw-name'>{t('channels.rawHTTP.name')}</Label>
            <Input id='raw-name' required value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div className='space-y-2'>
            <Label htmlFor='raw-base-url'>{t('channels.rawHTTP.baseURL')}</Label>
            <Input
              id='raw-base-url'
              type='url'
              required
              placeholder='https://api.example.com/v1'
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
            />
          </div>
          <div className='space-y-2'>
            <Label htmlFor='raw-resource'>{t('channels.rawHTTP.resource')}</Label>
            <Input id='raw-resource' required placeholder='exa-search' value={resource} onChange={(e) => setResource(e.target.value)} />
            <p className='text-muted-foreground text-sm'>{t('channels.rawHTTP.resourceHelp')}</p>
          </div>
          <div className='grid grid-cols-2 gap-4'>
            <div className='space-y-2'>
              <Label>{t('channels.rawHTTP.authentication')}</Label>
              <Select value={authType} onValueChange={(v) => setAuthType(v as 'bearer' | 'header')}>
                <SelectTrigger data-testid='raw-auth'>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value='bearer'>Bearer</SelectItem>
                  <SelectItem value='header'>{t('channels.rawHTTP.header')}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            {authType === 'header' && (
              <div className='space-y-2'>
                <Label htmlFor='raw-header'>{t('channels.rawHTTP.headerName')}</Label>
                <Input
                  id='raw-header'
                  required
                  placeholder='x-api-key'
                  value={authHeader}
                  onChange={(e) => setAuthHeader(e.target.value)}
                />
              </div>
            )}
          </div>
          <div className='space-y-2'>
            <Label htmlFor='raw-key'>{t('channels.rawHTTP.apiKey')}</Label>
            <Input
              id='raw-key'
              type='password'
              autoComplete='new-password'
              required={!editing}
              value={apiKey}
              onChange={(e) => setAPIKey(e.target.value)}
              placeholder={editing ? t('channels.rawHTTP.keepKey') : ''}
            />
          </div>
          {editing && (
            <div className='bg-muted rounded-md px-3 py-2 font-mono text-sm break-all' data-testid='raw-route'>
              /raw/{extractNumberID(currentRow.id)}/*path
            </div>
          )}
        </form>
        <DialogFooter>
          <Button type='button' variant='outline' onClick={() => onOpenChange(false)}>
            {t('common.buttons.cancel')}
          </Button>
          <Button type='submit' form='raw-http-form' disabled={pending}>
            {t('common.buttons.save')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
