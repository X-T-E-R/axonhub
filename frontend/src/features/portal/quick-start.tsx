import { useEffect, useRef, useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Label } from '@/components/ui/label';
import { CodeBlock } from '@/components/ai-elements/code-block';
import {
  type PortalKey,
  type PortalModel,
  type PortalModelChannel,
  portalRequest,
  MODELS_QUERY,
  MODEL_CHANNELS_QUERY,
  usePortalQuery,
} from './data';
import { usePortalGroup } from './layout';
import { accessTemplates } from './templates';

export function QuickStart({ keys, initialModel, channelID }: { keys: PortalKey[]; initialModel?: PortalModel; channelID?: string }) {
  const { t } = useTranslation();
  const group = usePortalGroup();
  const enabledKeys = keys.filter((k) => k.status === 'enabled');
  const [keyID, setKeyID] = useState<number | null>(enabledKeys[0]?.id ?? null);
  const models = usePortalQuery<{ models: PortalModel[] }>(
    'key-models',
    MODELS_QUERY,
    { projectID: group?.id, keyID },
    !!group?.member && !channelID
  );
  const channels = usePortalQuery<{ modelChannels: PortalModelChannel[] }>(
    'key-model-channels',
    MODEL_CHANNELS_QUERY,
    { projectID: group?.id, keyID },
    !!group?.member && !!channelID
  );
  const availableModels = channelID
    ? channels.data?.modelChannels.find((channel) => channel.id === channelID)?.models
    : models.data?.models;
  const [modelID, setModelID] = useState(initialModel?.id || '');
  const [templateID, setTemplateID] = useState('');
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  useEffect(() => {
    if (!enabledKeys.some((k) => k.id === keyID)) setKeyID(enabledKeys[0]?.id ?? null);
  }, [keys, keyID]);
  const model = availableModels?.find((m) => m.id === modelID) || availableModels?.[0];
  const templates = model ? accessTemplates(model) : [];
  const template = templates.find((tpl) => tpl.id === templateID) || templates[0];
  const selectedKey = enabledKeys.find((k) => k.id === keyID);
  const base = window.location.origin;
  async function copyCode() {
    try {
      const { revealKey } = selectedKey
        ? await portalRequest<{ revealKey: string }>('mutation Reveal($id: Int!) { revealKey(id: $id) }', { id: selectedKey.id })
        : { revealKey: '$AXONHUB_API_KEY' };
      if (!mounted.current || !template || !model) return;
      await navigator.clipboard.writeText(template.code(base, model.id, revealKey));
      toast.success(t('portal.copied'));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t('portal.copyFailed'));
    }
  }
  return (
    <Card className='min-w-0 rounded-2xl'>
      <CardHeader>
        <CardTitle>{t('portal.quickStart')}</CardTitle>
      </CardHeader>
      <CardContent className='grid min-w-0 gap-6 md:grid-cols-[minmax(180px,1fr)_minmax(0,2fr)]'>
        <div className='space-y-4'>
          <div className='space-y-2'>
            <Label htmlFor='quick-key'>{t('portal.key')}</Label>
            <select
              id='quick-key'
              className='bg-background w-full rounded-md border p-2 text-sm'
              value={keyID ?? ''}
              onChange={(e) => {
                setKeyID(Number(e.target.value));
                setModelID('');
                setTemplateID('');
              }}
            >
              <option value='' disabled>
                {t('portal.noKeys')}
              </option>
              {enabledKeys.map((k) => (
                <option key={k.id} value={k.id}>
                  {k.name}
                </option>
              ))}
            </select>
          </div>
          {!enabledKeys.length && (
            <Button asChild>
              <Link to='/portal/keys'>{t('portal.createKey')}</Link>
            </Button>
          )}
          <div className='space-y-2'>
            <Label htmlFor='quick-model'>{t('portal.model')}</Label>
            <select
              id='quick-model'
              className='bg-background w-full rounded-md border p-2 text-sm'
              value={model?.id || ''}
              onChange={(e) => {
                setModelID(e.target.value);
                setTemplateID('');
              }}
            >
              <option value='' disabled>
                {t('portal.noModels')}
              </option>
              {availableModels?.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.id}
                </option>
              ))}
            </select>
          </div>
          <div>
            <Label>{t('portal.baseURL')}</Label>
            <p className='text-muted-foreground mt-2 font-mono text-xs break-all'>{base}</p>
          </div>
        </div>
        <div className='min-w-0 space-y-3'>
          {templates.length > 0 ? (
            <>
              <div className='flex flex-wrap items-center gap-2'>
                <Label htmlFor='quick-protocol' className='sr-only'>
                  {t('portal.protocol')}
                </Label>
                <select
                  id='quick-protocol'
                  value={template?.id}
                  onChange={(e) => setTemplateID(e.target.value)}
                  className='bg-background rounded-md border p-2 text-sm'
                >
                  {templates.map((tpl) => (
                    <option key={tpl.id} value={tpl.id}>
                      {tpl.label}
                    </option>
                  ))}
                </select>
                <Button size='sm' variant='outline' onClick={() => void copyCode()}>
                  {t('portal.copyCode')}
                </Button>
              </div>
              <CodeBlock
                key={`${model?.id}:${template.id}:${selectedKey?.id}`}
                code={template.code(base, model!.id, selectedKey?.maskedKey || '$AXONHUB_API_KEY')}
                language={template.language}
                className='max-w-full overflow-x-auto'
              />
            </>
          ) : (
            <p className='text-muted-foreground py-8 text-sm'>
              {(channelID ? channels.error : models.error)?.message ||
                t((channelID ? channels.isLoading : models.isLoading) ? 'portal.models' : 'portal.noTemplate')}
            </p>
          )}
        </div>
      </CardContent>
    </Card>
  );
}
