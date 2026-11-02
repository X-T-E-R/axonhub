import { Copy, Eye } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';

export function KeyCredentialCell({
  maskedKey,
  onCopy,
  onView,
  disabled = false,
}: {
  maskedKey: string;
  onCopy: () => void | Promise<void>;
  onView?: () => void;
  disabled?: boolean;
}) {
  const { t } = useTranslation();
  return (
    <div className='flex max-w-48 items-center space-x-2'>
      <code className='bg-muted truncate rounded px-2 py-1 font-mono text-sm'>{maskedKey}</code>
      {onView && (
        <Button variant='ghost' size='sm' onClick={onView} className='h-6 w-6 shrink-0 p-0' aria-label={t('apikeys.actions.view')}>
          <Eye className='h-3 w-3' />
        </Button>
      )}
      <Button
        variant='ghost'
        size='sm'
        onClick={() => void onCopy()}
        disabled={disabled}
        className='h-6 w-6 shrink-0 p-0'
        aria-label={t('apikeys.actions.copy')}
      >
        <Copy className='h-3 w-3' />
      </Button>
    </div>
  );
}
