// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import React from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { AppShell } from '@/components/AppShell';
import { ThemeProvider } from '@/components/ThemeProvider';
import OverviewPage from '@/pages/Overview';
import SandboxesPage from '@/pages/Sandboxes';
import SandboxDetailPage from '@/pages/SandboxDetail';
import TemplatesPage from '@/pages/Templates';
import NodesPage from '@/pages/Nodes';
import KeysPage from '@/pages/Keys';
import { Placeholder } from '@/pages/Placeholder';
import { Network, Activity, Settings, Package, Plus } from 'lucide-react';

import './styles/globals.css';
import '@/i18n';
import { isMockEnabled } from '@/lib/mockFlag';

const qc = new QueryClient({
  defaultOptions: {
    queries: { retry: 1, refetchOnWindowFocus: false, staleTime: 2_000 },
  },
});

const App = () => (
  <React.StrictMode>
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <BrowserRouter>
          <Routes>
            <Route element={<AppShell />}>
              <Route path="/" element={<OverviewPage />} />
              <Route path="/sandboxes" element={<SandboxesPage />} />
              <Route
                path="/sandboxes/new"
                element={<Placeholder titleKey="newSandbox.title" descriptionKey="newSandbox.description" icon={Plus} />}
              />
              <Route path="/sandboxes/:sandboxID" element={<SandboxDetailPage />} />
              <Route path="/templates" element={<TemplatesPage />} />
              <Route
                path="/templates/:templateID"
                element={<Placeholder titleKey="templateDetail.title" descriptionKey="templateDetail.description" icon={Package} />}
              />
              <Route path="/nodes" element={<NodesPage />} />
              <Route
                path="/network"
                element={<Placeholder titleKey="network.title" descriptionKey="network.description" icon={Network} />}
              />
              <Route
                path="/observability"
                element={<Placeholder titleKey="observability.title" descriptionKey="observability.description" icon={Activity} />}
              />
              <Route path="/keys" element={<KeysPage />} />
              <Route
                path="/settings"
                element={<Placeholder titleKey="settings.title" descriptionKey="settings.description" icon={Settings} />}
              />
              <Route path="*" element={<Navigate to="/" replace />} />
            </Route>
          </Routes>
        </BrowserRouter>
      </ThemeProvider>
    </QueryClientProvider>
  </React.StrictMode>
);

async function bootstrap() {
  if (import.meta.env.DEV && isMockEnabled()) {
    const { enableMocking } = await import('@/mocks/browser');
    await enableMocking();
  }

  ReactDOM.createRoot(document.getElementById('root')!).render(<App />);
}

void bootstrap();
