import type { ReactNode } from 'react';
import PageHeader from './PageHeader.jsx';

interface PageScaffoldProps {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  filters?: ReactNode;
  summary?: ReactNode;
  children: ReactNode;
  drawer?: ReactNode;
  ready?: boolean;
  className?: string;
}

export default function PageScaffold({
  title,
  description,
  actions,
  filters,
  summary,
  children,
  drawer,
  ready = true,
  className = '',
}: PageScaffoldProps) {
  return (
    <div className={`pool-page ${className}`.trim()} data-page-section-ready={ready ? 'true' : 'false'}>
      <PageHeader title={title} subtitle={description} actions={actions} />
      {filters ? <section className="pool-page__filters" aria-label="筛选与工具">{filters}</section> : null}
      {summary ? <section className="pool-page__summary" aria-label="摘要">{summary}</section> : null}
      <section className="pool-page__content">{children}</section>
      {drawer}
    </div>
  );
}
