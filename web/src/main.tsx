// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import '@fontsource-variable/inter';
import '@fontsource-variable/jetbrains-mono';

import React from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { AppShell } from '@/components/AppShell';
import { ThemeProvider } from '@/components/ThemeProvider';
import OverviewPage from '@/pages/Overview';
import SandboxesPage from '@/pages/Sandboxes';
import SandboxDetailPage from '@/pages/SandboxDetail';
import SandboxNewPage from '@/pages/SandboxNew';
import TemplatesPage from '@/pages/Templates';
import NodesPage from '@/pages/Nodes';
import VersionsPage from '@/pages/Versions';
import KeysPage from '@/pages/Keys';
import SettingsPage from '@/pages/Settings';
import TemplateDetailPage from '@/pages/TemplateDetail';
import NodeDetailPage from '@/pages/NodeDetail';
import NetworkPage from '@/pages/Network';
import ObservabilityPage from '@/pages/Observability';
import TemplateStorePage from '@/pages/TemplateStore';
import AgentHubPage from '@/pages/AgentHub';
import LoginPage from '@/pages/Login';
import { AuthGuard } from '@/components/AuthGuard';
import ExamplesPage from '@/pages/SandboxCases';
import { Placeholder } from '@/pages/Placeholder';
import { Network, Activity, Settings, Package } from 'lucide-react';

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
            <Route path="/login" element={<LoginPage />} />
            <Route element={<AuthGuard />}>
              <Route element={<AppShell />}>
              <Route path="/" element={<OverviewPage />} />
              <Route path="/sandboxes" element={<SandboxesPage />} />
              <Route path="/sandboxes/new" element={<SandboxNewPage />} />
              <Route path="/sandboxes/:sandboxID" element={<SandboxDetailPage />} />
              <Route path="/templates" element={<TemplatesPage />} />
              <Route
                path="/templates/:templateID"
                element={<TemplateDetailPage />}
              />
              <Route path="/nodes" element={<NodesPage />} />
              <Route path="/nodes/:nodeID" element={<NodeDetailPage />} />
              <Route path="/versions" element={<VersionsPage />} />
              <Route path="/network" element={<NetworkPage />} />
              <Route path="/observability" element={<ObservabilityPage />} />
            <Route path="/store" element={<TemplateStorePage />} />
            <Route path="/agenthub" element={<AgentHubPage />} />
            <Route path="/examples" element={<ExamplesPage />} />
            <Route path="/keys" element={<KeysPage />} />
              <Route path="/settings" element={<SettingsPage />} />
              <Route path="*" element={<Navigate to="/" replace />} />
              </Route>
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
