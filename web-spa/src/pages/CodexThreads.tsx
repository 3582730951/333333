import React, { useEffect, useMemo, useState } from 'react';
import { Button, Select, Tag, Toast } from '../components/pool/index.jsx';
import { IconPlay, IconRefresh, IconStop } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { TextClamp } from '../components/DisplayPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import { middleEllipsis } from '../lib/format.js';
import { codexThreadErrorCode, interruptCodexThread, resumeCodexThread, subscribeCodexThreadEvents } from '../features/codexThreads/api';
import { useCodexRuntimes, useCodexThreadList } from '../features/codexThreads/queries';
import type { CodexThread, CodexThreadFilters } from '../features/codexThreads/types';

const DataTable = ResourceTable as any;
const MobileRow = MobileResourceCell as any;
const Clamp = TextClamp as any;

function statusColor(status: string): string {
  if (status === 'active') return 'blue';
  if (status === 'idle' || status === 'turnAborted') return 'green';
  if (status === 'systemError') return 'red';
  return 'grey';
}

function statusLabel(thread: CodexThread): string {
  if (thread.status !== 'active' || !thread.waitingReason) return thread.status || 'unknown';
  return `${thread.status} · ${thread.waitingReason}`;
}

function updatedAt(value?: string): string {
  if (!value) return '—';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '—' : new Intl.DateTimeFormat(undefined, {
    month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false,
  }).format(date);
}

function isStopEnabled(thread: CodexThread, stopping: string | null): boolean {
  return thread.status === 'active'
    && thread.runtimeAvailable
    && Boolean(thread.activeTurnHandle)
    && stopping === null;
}

function isResumeEligible(thread: CodexThread): boolean {
  return thread.status === 'active' || thread.status === 'idle' || thread.status === 'notLoaded';
}

export default function CodexThreads() {
  const runtimes = useCodexRuntimes();
  const [runtimeId, setRuntimeId] = useState('');
  const [searchTerm, setSearchTerm] = useState('');
  const [source, setSource] = useState('');
  const [selectedKeys, setSelectedKeys] = useState<string[]>([]);
  const [stopping, setStopping] = useState<string | null>(null);
  const [resuming, setResuming] = useState<string | null>(null);
  const [streamError, setStreamError] = useState('');

  const runtimeRows = runtimes.data || [];
  useEffect(() => {
    if (runtimeId && runtimeRows.some((runtime) => runtime.id === runtimeId)) return;
    setRuntimeId(runtimeRows.find((runtime) => runtime.available)?.id || runtimeRows[0]?.id || '');
  }, [runtimeId, runtimeRows]);

  const filters = useMemo<CodexThreadFilters>(() => ({
    runtimeId,
    searchTerm,
    sourceKinds: source ? [source] : undefined,
    sortKey: 'updated_at',
    sortDirection: 'desc',
  }), [runtimeId, searchTerm, source]);
  const list = useCodexThreadList(filters);
  const currentRuntime = runtimeRows.find((runtime) => runtime.id === runtimeId);

  useEffect(() => {
    setSelectedKeys((current) => current.filter((key) => list.rows.some((thread) => thread.threadKey === key)));
  }, [list.rows]);

  useEffect(() => {
    if (!runtimeId || !currentRuntime?.available) return undefined;
    const controller = new AbortController();
    setStreamError('');
    void subscribeCodexThreadEvents({
      runtimeId,
      signal: controller.signal,
      onStatus: list.patchStatus,
    }).catch((error) => {
      if (controller.signal.aborted) return;
      setStreamError(codexThreadErrorCode(error) || '实时状态连接不可用；可继续手动刷新。');
    });
    return () => controller.abort();
  }, [currentRuntime?.available, list.patchStatus, runtimeId]);

  const stop = async (thread: CodexThread) => {
    const captured = {
      threadHandle: thread.threadHandle,
      turnHandle: thread.activeTurnHandle || '',
      revision: thread.revision,
      key: thread.threadKey,
    };
    if (!captured.turnHandle || !isStopEnabled(thread, stopping)) return;
    if (!window.confirm(`确认停止当前运行轮次（版本 ${captured.revision}）？`)) return;
    setStopping(captured.key);
    try {
      await interruptCodexThread(captured.threadHandle, captured.turnHandle);
      Toast.success('已确认停止；正在刷新线程状态。');
      await list.reload();
    } catch (error) {
      const code = codexThreadErrorCode(error);
      if (code === 'codex_stale_turn' || code === 'codex_no_active_turn') {
        Toast.warning('运行轮次已变化，请刷新。');
        await list.reload();
      } else {
        showErrorToast(error);
      }
    } finally {
      setStopping((current) => current === captured.key ? null : current);
    }
  };

  const resume = async (thread: CodexThread) => {
    if (!thread.runtimeAvailable || !isResumeEligible(thread) || resuming) return;
    setResuming(thread.threadKey);
    try {
      await resumeCodexThread(thread.threadHandle);
      Toast.success(thread.status === 'active' ? '已重新加入运行线程。' : '已恢复线程。');
      await list.reload();
    } catch (error) {
      showErrorToast(error);
    } finally {
      setResuming((current) => current === thread.threadKey ? null : current);
    }
  };

  const actions = (thread: CodexThread) => {
    const isStopping = stopping === thread.threadKey;
    const isResuming = resuming === thread.threadKey;
    const stopAllowed = isStopEnabled(thread, stopping);
    const resumeAllowed = thread.runtimeAvailable && isResumeEligible(thread) && !resuming && !isStopping;
    return <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
      <Button
        size="small"
        icon={<IconPlay />}
        loading={isResuming}
        disabled={!resumeAllowed}
        title={!thread.runtimeAvailable ? '运行时不可用' : undefined}
        onClick={() => void resume(thread)}
      >{thread.status === 'active' ? 'Rejoin' : 'Resume'}</Button>
      <Button
        size="small"
        type="danger"
        icon={<IconStop />}
        loading={isStopping}
        disabled={!stopAllowed}
        title={!thread.runtimeAvailable ? '运行时不可用' : !thread.activeTurnHandle ? '没有可停止的活动轮次' : undefined}
        onClick={() => void stop(thread)}
      >Stop</Button>
    </div>;
  };

  const columns: any[] = [
    { title: 'Runtime', dataIndex: 'runtimeLabel', width: 140, render: (_: unknown, row: CodexThread) => <Clamp title={row.runtimeLabel || row.runtimeId}>{row.runtimeLabel || row.runtimeId}</Clamp> },
    { title: 'Thread', dataIndex: 'threadHandle', width: 150, render: (value: string) => <span className="pool-mono" title={value}>{middleEllipsis(value, 8, 8)}</span> },
    { title: 'Model / provider', key: 'model', width: 150, render: (_: unknown, row: CodexThread) => <><Tag>{row.model || 'unknown'}</Tag>{row.modelProvider ? <span className="pool-muted"> {row.modelProvider}</span> : null}</> },
    { title: 'Source', dataIndex: 'source', width: 90, render: (value: string) => value ? <Tag>{value}</Tag> : '—' },
    { title: 'Status', dataIndex: 'status', width: 185, render: (_: unknown, row: CodexThread) => <Tag color={statusColor(row.status)}>{statusLabel(row)}</Tag> },
    { title: 'Active turn', dataIndex: 'activeTurnHandle', width: 135, priority: 10, render: (value: string) => value ? <span className="pool-mono" title={value}>{middleEllipsis(value, 7, 7)}</span> : '—' },
    { title: 'Runtime', dataIndex: 'runtimeAvailable', width: 105, priority: 15, render: (value: boolean) => <Tag color={value ? 'green' : 'red'}>{value ? 'available' : 'unavailable'}</Tag> },
    { title: 'Updated', dataIndex: 'updatedAt', width: 135, priority: 20, render: (value: string) => updatedAt(value) },
    { title: 'Direct input', dataIndex: 'directInput', width: 100, priority: 25, render: (value: boolean) => value ? <Tag color="blue">yes</Tag> : '—' },
    { title: 'Actions', key: 'actions', width: 178, render: (_: unknown, row: CodexThread) => actions(row) },
  ];
  const mobileColumns: any[] = [{
    title: 'Codex thread', dataIndex: 'threadHandle', render: (_: unknown, row: CodexThread) => <MobileRow
      title={middleEllipsis(row.threadHandle, 10, 8)}
      subtitle={`${row.runtimeLabel || row.runtimeId} · ${row.model || 'unknown'}`}
      badges={<><Tag color={statusColor(row.status)}>{statusLabel(row)}</Tag>{row.runtimeAvailable ? <Tag color="green">available</Tag> : <Tag color="red">unavailable</Tag>}</>}
      details={[
        { label: 'Source', value: row.source || '—' },
        { label: 'Updated', value: updatedAt(row.updatedAt) },
        { label: 'Direct input', value: row.directInput ? 'yes' : 'no' },
      ]}
      actions={actions(row)}
    />,
  }];

  return (
    <div>
      <PageHeader
        title="Codex Threads"
        subtitle="独立的 app-server 线程控制面；只显示安全元数据与短期 opaque handle。"
        actions={<>
          <Select
            aria-label="Codex runtime"
            value={runtimeId}
            disabled={runtimes.isLoading || !runtimeRows.length}
            optionList={runtimeRows.map((runtime) => ({ label: `${runtime.label || runtime.id}${runtime.available ? '' : ' · unavailable'}`, value: runtime.id, disabled: !runtime.available }))}
            onChange={(value: string) => setRuntimeId(value)}
            placeholder="选择运行时"
            style={{ width: 210 }}
          />
          <Button icon={<IconRefresh />} loading={list.refreshing || runtimes.isFetching} onClick={() => { void runtimes.refetch(); void list.reload(); }}>刷新</Button>
        </>}
      />

      <div className="pool-toolbar" style={{ display: 'flex', gap: 10, flexWrap: 'wrap', marginBottom: 14 }}>
        <label>
          <span className="pool-muted" style={{ display: 'block', fontSize: 'var(--pool-type-caption)', marginBottom: 4 }}>来源</span>
          <Select value={source} onChange={(value: string) => setSource(value)} optionList={[{ label: '全部来源', value: '' }, { label: 'CLI', value: 'cli' }, { label: 'User', value: 'user' }, { label: 'Rollout', value: 'rollout' }, { label: 'Imported', value: 'imported' }]} style={{ width: 150 }} />
        </label>
        <label style={{ minWidth: 230, flex: '1 1 260px' }}>
          <span className="pool-muted" style={{ display: 'block', fontSize: 'var(--pool-type-caption)', marginBottom: 4 }}>安全搜索</span>
          <input className="pool-input" value={searchTerm} maxLength={256} placeholder="按 app-server 元数据搜索" onChange={(event) => setSearchTerm(event.target.value)} />
        </label>
      </div>

      {streamError ? <p className="pool-muted" role="status" style={{ marginTop: 0 }}>{streamError}</p> : null}
      <DataTable
        error={list.error || runtimes.error}
        onRetry={() => { void runtimes.refetch(); void list.reload(); }}
        loading={list.loading || runtimes.isLoading}
        lastRefresh={list.lastRefresh}
        dataSource={list.rows}
        columns={columns}
        mobileColumns={mobileColumns}
        rowKey="threadKey"
        rowSelection={{ selectedRowKeys: selectedKeys, onChange: (keys: string[]) => setSelectedKeys(keys) }}
        pagination={false}
        className="pool-mobile-table pool-codex-threads-table"
        emptyTitle={runtimeId ? '该运行时暂无可见线程' : '请选择可用 Codex runtime'}
        emptyDesc="线程内容、提示词、完整路径和原始上游 ID 不会显示在这里。"
        emptyType="default"
        skeletonRows={6}
        scroll={false}
      />
      {list.nextCursor ? <div style={{ marginTop: 12 }}><Button disabled={list.refreshing} onClick={() => void list.loadNext()}>加载更多</Button></div> : null}
    </div>
  );
}
