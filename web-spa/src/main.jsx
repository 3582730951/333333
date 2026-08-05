import React, { useState, useEffect } from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter } from 'react-router';
import { QueryClientProvider } from '@tanstack/react-query';
import App from './App.tsx';
import { AuthProvider } from './app/AuthProvider.tsx';
import { queryClient } from './app/queryClient.ts';
import AppErrorBoundary, { installGlobalErrorHandlers } from './components/AppErrorBoundary.jsx';
import AppUpdateNotice from './components/AppUpdateNotice.jsx';
import { ToastViewport } from './components/pool/index.jsx';
import { requireDocumentElement } from './lib/browserDocument.js';
import { getLocalItem } from './lib/browserStorage.js';
import { addWindowListener } from './lib/browserLifecycle.js';
import { getLocale } from './lib/i18n.js';
import './styles/tokens.css';
import './styles/base.css';
import './styles/layout.css';
import './styles/components.css';
import './styles/utilities.css';
import './styles/apple-ui.css';

try {
  const preference = getLocalItem('pool_theme', 'auto') || 'auto';
  const resolved = preference === 'auto'
    ? (window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')
    : preference;
  document.documentElement.dataset.theme = resolved === 'dark' ? 'dark' : 'light';
  document.documentElement.dataset.themePreference = preference;
} catch {
  document.documentElement.dataset.theme = 'light';
}

const disposeGlobalErrorHandlers = installGlobalErrorHandlers();
if (import.meta.hot) {
  import.meta.hot.dispose(disposeGlobalErrorHandlers);
}

function Root() {
  const [, setRenderedLocale] = useState(getLocale());
  useEffect(() => {
    const handler = (e) => setRenderedLocale(e.detail);
    return addWindowListener('pool-locale-change', handler);
  }, []);
  return (
    <AppErrorBoundary>
      <QueryClientProvider client={queryClient}>
        <AuthProvider>
          <BrowserRouter
            basename="/console"
            future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
          >
            <App />
          </BrowserRouter>
        </AuthProvider>
      </QueryClientProvider>
      <AppUpdateNotice />
      <ToastViewport />
    </AppErrorBoundary>
  );
}

ReactDOM.createRoot(requireDocumentElement('root')).render(
  <React.StrictMode>
    <Root />
  </React.StrictMode>
);
