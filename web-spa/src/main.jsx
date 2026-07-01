import React, { useState, useEffect } from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import { LocaleProvider } from '@douyinfe/semi-ui';
import zh_CN from '@douyinfe/semi-ui/lib/es/locale/source/zh_CN';
import en_US from '@douyinfe/semi-ui/lib/es/locale/source/en_US';
import App from './App.jsx';
import AppErrorBoundary, { installGlobalErrorHandlers } from './components/AppErrorBoundary.jsx';
import AppUpdateNotice from './components/AppUpdateNotice.jsx';
import { requireDocumentElement } from './lib/browserDocument.js';
import { addWindowListener } from './lib/browserLifecycle.js';
import { getLocale } from './lib/i18n.js';
import './theme.css';

const disposeGlobalErrorHandlers = installGlobalErrorHandlers();
if (import.meta.hot) {
  import.meta.hot.dispose(disposeGlobalErrorHandlers);
}

function Root() {
  const [semiLocale, setSemiLocale] = useState(getLocale() === 'en' ? en_US : zh_CN);
  useEffect(() => {
    const handler = (e) => setSemiLocale(e.detail === 'en' ? en_US : zh_CN);
    return addWindowListener('pool-locale-change', handler);
  }, []);
  return (
    <LocaleProvider locale={semiLocale}>
      <AppErrorBoundary>
        <BrowserRouter basename="/console">
          <App />
        </BrowserRouter>
        <AppUpdateNotice />
      </AppErrorBoundary>
    </LocaleProvider>
  );
}

ReactDOM.createRoot(requireDocumentElement('root')).render(
  <React.StrictMode>
    <Root />
  </React.StrictMode>
);
