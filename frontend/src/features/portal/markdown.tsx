import { Fragment } from 'react';
import ReactMarkdown, { defaultUrlTransform } from 'react-markdown';
import { bundledLanguages, type BundledLanguage } from 'shiki';
import { CodeBlock, CodeBlockCopyButton } from '@/components/ai-elements/code-block';

export function safePortalURL(value: string): string {
  const safe = defaultUrlTransform(value);
  if (!safe || /^(?:javascript|vbscript|data|file):/i.test(safe.replace(/[\u0000-\u0020]/g, ''))) return '';
  return /^(?:https?:|mailto:|\/|#|\.{1,2}\/)/i.test(safe) || !safe.includes(':') ? safe : '';
}
export function PortalMarkdown({ children }: { children: string }) {
  return (
    <div className='prose prose-sm dark:prose-invert max-w-none break-words [&_pre]:max-w-full [&_pre]:overflow-x-auto'>
      <ReactMarkdown
        skipHtml
        urlTransform={safePortalURL}
        disallowedElements={['img', 'iframe', 'script', 'style', 'object', 'embed', 'form', 'input']}
        components={{
          a: ({ href, children }) => (
            <a href={href} target='_blank' rel='noopener noreferrer'>
              {children}
            </a>
          ),
          pre: ({ children }) => <Fragment>{children}</Fragment>,
          code: ({ className, children }) => {
            const name = /language-([^\s]+)/.exec(className || '')?.[1];
            if (!name) return <code className='bg-muted rounded px-1 py-0.5 font-mono'>{children}</code>;
            const language: BundledLanguage = Object.prototype.hasOwnProperty.call(bundledLanguages, name)
              ? (name as BundledLanguage)
              : ('text' as BundledLanguage);
            return (
              <CodeBlock code={String(children).replace(/\n$/, '')} language={language} className='my-4 overflow-x-auto'>
                <CodeBlockCopyButton aria-label='Copy code' />
              </CodeBlock>
            );
          },
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}
