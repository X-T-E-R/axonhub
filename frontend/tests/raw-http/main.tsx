// Standalone browser fixture: production dialog/hooks/i18n with real loopback
// JWT/GraphQL/Ent. No provider requests and no user database are used.
import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Toaster } from 'sonner';
import '@/index.css';
import '@/lib/i18n';
import { setTokenToStorage } from '@/stores/authStore';
import { ChannelsRawHTTPDialog } from '@/features/channels/components/channels-raw-http-dialog';
import { useQueryChannels } from '@/features/channels/data/channels';
import { Channel } from '@/features/channels/data/schema';
import { Button } from '@/components/ui/button';

const fixture = await (await fetch('/__raw_fixture')).json();
setTokenToStorage(fixture.token);

function App() {
  const rows = useQueryChannels({ first: 50 });
  const [open, setOpen] = useState(false);
  const [row, setRow] = useState<Channel>();

  return (
    <main className='mx-auto max-w-4xl space-y-6 p-10'>
      <h1 className='text-2xl font-semibold'>Raw HTTP channels · isolated fixture</h1>
      <Button
        onClick={() => {
          setRow(undefined);
          setOpen(true);
        }}
      >
        Add HTTP API
      </Button>
      <div className='space-y-3'>
        {rows.data?.edges?.map(
          (edge) =>
            edge?.node.settings?.rawHTTP?.enabled && (
              <div
                key={edge.node.id}
                className='flex items-center justify-between rounded-md border p-4'
                data-testid='raw-saved-row'
              >
                <div>
                  <strong>{edge.node.name}</strong>
                  <div>{edge.node.baseURL}</div>
                  <div>
                    {edge.node.settings.rawHTTP.resource} · {edge.node.settings.rawHTTP.authType}
                  </div>
                </div>
                <Button
                  onClick={() => {
                    setRow(edge.node);
                    setOpen(true);
                  }}
                >
                  Edit {edge.node.name}
                </Button>
              </div>
            )
        )}
      </div>
      {open && <ChannelsRawHTTPDialog key={row?.id ?? 'create'} open currentRow={row} onOpenChange={setOpen} />}
      <Toaster />
    </main>
  );
}

createRoot(document.getElementById('root')!).render(
  <QueryClientProvider client={new QueryClient()}>
    <App />
  </QueryClientProvider>
);
