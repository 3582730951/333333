import React, { useState, useEffect, useCallback } from 'react';
import {
  Button, Card, Form, Input, Tag, Toast, Typography,
} from '../../../../components/pool/index.jsx';
import { IconRefresh, IconPlay, IconStop } from '../../../../components/pool/icons.jsx';
import PageHeader from '../../../../components/PageHeader.jsx';
import PageScaffold from '../../../../components/PageScaffold.tsx';
import useAsyncAction from '../../../../hooks/useAsyncAction.jsx';
import {
  fetchEmailRegConfig, saveEmailRegConfig, startEmailRegistration,
  fetchEmailRegJobs, fetchEmailRegJobStatus, cancelEmailRegJob,
} from '../../api/emailRegistration';
import { fetchEmailPool } from '../../../accounts/api/emailPool';
import type { EmailRegSettings, EmailRegJob } from '../../api/emailRegistration';

export default function EmailRegistrationPage() {
  const [config, setConfig] = useState<EmailRegSettings>({ count: 1, group_name: 'cyber', concurrency: 2, egress_pool_id: '' });
  const [idleCount, setIdleCount] = useState(0);
  const [jobs, setJobs] = useState<EmailRegJob[]>([]);
  const [expandedJob, setExpandedJob] = useState<string | null>(null);
  const [jobDetail, setJobDetail] = useState<EmailRegJob | null>(null);

  const loadConfig = useCallback(async () => {
    try {
      const c = await fetchEmailRegConfig();
      setConfig(c);
    } catch {
      // Use defaults
    }
  }, []);

  const loadIdleCount = useCallback(async () => {
    try {
      const result = await fetchEmailPool({ pageSize: 1, status: 'idle' });
      setIdleCount(result.total);
    } catch {
      setIdleCount(0);
    }
  }, []);

  const loadJobs = useCallback(async () => {
    try {
      const result = await fetchEmailRegJobs(50);
      setJobs(result.jobs || []);
    } catch {
      setJobs([]);
    }
  }, []);

  const loadAll = useCallback(async () => {
    await Promise.all([loadConfig(), loadIdleCount(), loadJobs()]);
  }, [loadConfig, loadIdleCount, loadJobs]);

  const { run: refresh, running: refreshing } = useAsyncAction(loadAll);

  const { run: saveConfig, running: saving } = useAsyncAction(async () => {
    await saveEmailRegConfig(config);
    Toast.success('Settings saved');
  });

  const { run: startReg, running: starting } = useAsyncAction(async () => {
    const result = await startEmailRegistration(config);
    Toast.success(`Registration started: ${result.job_id}`);
    await loadJobs();
    await loadIdleCount();
  });

  const { run: cancelJob } = useAsyncAction(async (jobId: string) => {
    await cancelEmailRegJob(jobId);
    Toast.success(`Job ${jobId} cancelled`);
    await loadJobs();
  });

  const { run: loadJobDetail } = useAsyncAction(async (jobId: string) => {
    const detail = await fetchEmailRegJobStatus(jobId);
    setJobDetail(detail);
  });

  useEffect(() => { refresh(); }, [refresh]);

  // Auto-refresh every 10 seconds
  useEffect(() => {
    const timer = setInterval(() => { loadJobs(); loadIdleCount(); }, 10000);
    return () => clearInterval(timer);
  }, [loadJobs, loadIdleCount]);

  const hasRunningJob = jobs.some(j => j.status === 'running');

  return (
    <PageScaffold>
      <PageHeader
        title="Email Registration"
        description="Register ChatGPT accounts using email OTP via Outlook/IMAP"
        actions={
          <Button onClick={refresh} loading={refreshing}>
            <IconRefresh />
          </Button>
        }
      />

      {/* Configuration Card */}
      <Card style={{ marginBottom: 24 }}>
        <Typography variant="heading" style={{ marginBottom: 16 }}>Configuration</Typography>
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: 16 }}>
          <Form.Item label="Number of Accounts">
            <Input
              type="number"
              min={1}
              max={50}
              value={String(config.count)}
              onChange={(e: React.ChangeEvent<HTMLInputElement>) => setConfig(c => ({ ...c, count: parseInt(e.target.value) || 1 }))}
            />
          </Form.Item>
          <Form.Item label="Concurrency">
            <Input
              type="number"
              min={1}
              max={10}
              value={String(config.concurrency)}
              onChange={(e: React.ChangeEvent<HTMLInputElement>) => setConfig(c => ({ ...c, concurrency: parseInt(e.target.value) || 2 }))}
            />
          </Form.Item>
          <Form.Item label="Target Group">
            <Input
              value={config.group_name}
              onChange={(e: React.ChangeEvent<HTMLInputElement>) => setConfig(c => ({ ...c, group_name: e.target.value }))}
              placeholder="cyber"
            />
          </Form.Item>
          <Form.Item label="Egress Pool ID (optional)">
            <Input
              value={config.egress_pool_id || ''}
              onChange={(e: React.ChangeEvent<HTMLInputElement>) => setConfig(c => ({ ...c, egress_pool_id: e.target.value }))}
              placeholder="Leave empty for direct"
            />
          </Form.Item>
        </div>

        {/* Status bar */}
        <div style={{ display: 'flex', gap: 16, marginTop: 16, alignItems: 'center' }}>
          <Typography variant="body">
            Idle email accounts available:{' '}
            <strong style={{ color: idleCount > 0 ? 'var(--pool-green)' : 'var(--pool-red)' }}>{idleCount}</strong>
          </Typography>
        </div>

        <div style={{ display: 'flex', gap: 8, marginTop: 16 }}>
          <Button onClick={saveConfig} loading={saving}>Save Settings</Button>
          <Button
            variant="primary"
            onClick={startReg}
            disabled={hasRunningJob || idleCount === 0}
            loading={starting}
          >
            <IconPlay /> Start Registration
          </Button>
        </div>
      </Card>

      {/* Jobs Table */}
      <Card>
        <Typography variant="heading" style={{ marginBottom: 16 }}>Registration Jobs</Typography>
        {jobs.length === 0 ? (
          <Typography variant="body" style={{ color: 'var(--pool-gray)' }}>
            No registration jobs yet. Configure and start a registration above.
          </Typography>
        ) : (
          <table className="pool-table" style={{ width: '100%' }}>
            <thead>
              <tr>
                <th>Job ID</th>
                <th>Status</th>
                <th>Progress</th>
                <th>Created</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {jobs.map((job) => (
                <React.Fragment key={job.id}>
                  <tr onClick={() => {
                    const newExpanded = expandedJob === job.id ? null : job.id;
                    setExpandedJob(newExpanded);
                    if (newExpanded) loadJobDetail(job.id);
                  }} style={{ cursor: 'pointer' }}>
                    <td style={{ fontFamily: 'monospace', fontSize: 12 }}>{job.id}</td>
                    <td>
                      <Tag color={
                        job.status === 'completed' ? 'green' :
                        job.status === 'running' ? 'blue' :
                        job.status === 'failed' ? 'red' : 'gray'
                      }>{job.status}</Tag>
                    </td>
                    <td>
                      {job.total > 0 ? `${job.succeeded}/${job.total}` : '-'}
                      {job.failed > 0 ? ` (${job.failed} failed)` : ''}
                    </td>
                    <td>{job.created_at ? new Date(job.created_at * 1000).toLocaleString() : '-'}</td>
                    <td>
                      {job.status === 'running' && (
                        <Button size="small" variant="danger" onClick={(e: React.MouseEvent) => { e.stopPropagation(); cancelJob(job.id); }}>
                          <IconStop /> Cancel
                        </Button>
                      )}
                    </td>
                  </tr>
                  {expandedJob === job.id && jobDetail && (
                    <tr>
                      <td colSpan={5} style={{ padding: 16, background: 'var(--pool-bg-surface)' }}>
                        <Typography variant="body">
                          Succeeded: {jobDetail.succeeded} | Failed: {jobDetail.failed} | Total: {jobDetail.total}
                        </Typography>
                        {jobDetail.error && (
                          <Typography variant="body" style={{ color: 'var(--pool-red)', marginTop: 4 }}>
                            Error: {jobDetail.error}
                          </Typography>
                        )}
                      </td>
                    </tr>
                  )}
                </React.Fragment>
              ))}
            </tbody>
          </table>
        )}
      </Card>
    </PageScaffold>
  );
}
