import React, { useState, useEffect } from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import App from './App.jsx';
import AppErrorBoundary, { installGlobalErrorHandlers } from './components/AppErrorBoundary.jsx';
import AppUpdateNotice from './components/AppUpdateNotice.jsx';
import { ToastViewport } from './components/pool/index.jsx';
import { requireDocumentElement } from './lib/browserDocument.js';
import { addWindowListener } from './lib/browserLifecycle.js';
import { getLocale } from './lib/i18n.js';
import './styles/tokens.css';
import './styles/base.css';
import './styles/layout.css';
import './styles/components.css';
import './styles/utilities.css';

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
      <BrowserRouter basename="/console">
        <App />
      </BrowserRouter>
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
