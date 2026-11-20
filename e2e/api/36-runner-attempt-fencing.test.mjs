// Destructive lease fault injection is restricted to an explicitly named
// disposable local MySQL container. This does not simulate a network outage.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { api, get, post, waitFor } from '../lib/api.mjs';
import { cfg, uid } from '../lib/env.mjs';

const container = process.env.AGENTFLOW_E2E_FENCING_MYSQL_CONTAINER || '';
const enabled = /^superagent-p0-fencing-[a-z0-9-]+$/.test(container)
  && ['127.0.0.1', 'localhost'].includes(new URL(cfg.base).hostname);

function expireLease(id) {
  assert.match(id, /^[a-zA-Z0-9_-]+$/);
  execFileSync('docker', ['exec', '-i', container, 'mysql', '-uroot', '-proot', 'agentflow'], {
    input: `UPDATE runner_tasks SET lease_until = DATE_SUB(UTC_TIMESTAMP(3), INTERVAL 1 SECOND) WHERE id = '${id}';`,
    stdio: ['pipe', 'pipe', 'pipe'],
  });
}

test('protocol v2: real HTTP/MySQL rejects stale attempts and preserves event generations', {
  skip: !enabled && 'requires an explicitly named disposable local fencing MySQL container', timeout: 120_000,
}, async () => {
  const sessions = [];
  const register = async label => {
    const { code } = await post('/api/runners/bind-codes', {});
    const registered = await api('POST', '/runners/register', {
      name: uid(label), os: process.platform, arch: process.arch, version: '0.4.0', maxConcurrency: 1,
      runtimes: [{ provider: 'fencing-test', binaryPath: '(test)', status: 'online' }],
    }, { auth: false, headers: { 'X-Bootstrap-Token': code } });
    sessions.push(registered.sessionToken);
    return registered;
  };
  try {
    const runner = await register('fencing-owner');
    const other = await register('fencing-other');
    const call = (method, path, body, expect = [200], token = runner.sessionToken, protocol = '2') => api(method, path, body, {
      auth: false, expect, raw: true,
      headers: { Authorization: `Bearer ${token}`, ...(protocol ? { 'X-Runner-Task-Protocol': protocol } : {}) },
    });
    const task = await post('/api/runners/tasks', { provider: 'fencing-test', prompt: 'disposable fencing probe', runnerId: runner.runnerId, maxAttempts: 3 });
    const path = `/runners/tasks/${task.id}`;
    const claim = { availableRuntimes: ['fencing-test'], capacity: 1, noWait: true };
    await call('POST', '/runners/tasks/claim', claim, [426], runner.sessionToken, '');
    assert.equal((await get('/api/runners/tasks')).find(t => t.id === task.id).attempt, 0, 'old Runner must not acquire lease');
    const firstResponse = await call('POST', '/runners/tasks/claim', claim);
    assert.equal(firstResponse.headers.get('X-Runner-Task-Protocol'), '2');
    const first = firstResponse.data.task;
    assert.equal(first.attempt, 1);
    await call('POST', path + '/start', { attempt: first.attempt });
    const event = { seq: 1, type: 'stdout', data: 'attempt one' };
    await call('POST', path + '/events', { attempt: first.attempt, events: [event] });
    expireLease(task.id);
    await call('POST', path + '/complete', { attempt: first.attempt, status: 'success', result: 'expired' }, [409]);
    await waitFor('sweeper requeues expired lease', async () => (await get('/api/runners/tasks')).find(t => t.id === task.id)?.status === 'queued', { timeoutMs: 35_000, intervalMs: 250 });
    const second = (await call('POST', '/runners/tasks/claim', claim)).data.task;
    assert.equal(second.id, first.id);
    assert.equal(second.attempt, 2);
    for (const attempt of [undefined, 0, -1, first.attempt, second.attempt + 1]) {
      const expected = attempt === undefined || attempt <= 0 ? [400] : [409];
      for (const [suffix, extra] of [['start', {}], ['lease/renew', {}], ['events', { events: [] }], ['complete', { status: 'success', result: 'stale' }]]) {
        await call('POST', `${path}/${suffix}`, { attempt, ...extra }, expected);
      }
      await call('GET', path + '/control' + (attempt === undefined ? '' : `?attempt=${attempt}`), undefined, expected);
    }
    await call('POST', path + '/complete', { attempt: second.attempt, status: 'success' }, [409], other.sessionToken);
    await call('POST', path + '/start', { attempt: second.attempt });
    await call('POST', path + '/lease/renew', { attempt: second.attempt });
    const batch = { attempt: second.attempt, events: [{ seq: 2, type: 'stdout', data: 'two second' }, { seq: 1, type: 'stdout', data: 'two first' }] };
    assert.equal((await call('POST', path + '/events', batch)).data.accepted, 2);
    assert.equal((await call('POST', path + '/events', batch)).data.accepted, 0);
    const history = (await call('GET', '/api' + path + '/events')).data;
    assert.deepEqual(history.map(e => [e.attempt, e.seq]), [[1, 1], [2, 1], [2, 2]]);
    await call('POST', path + '/complete', { attempt: second.attempt, status: 'success', result: 'current result' });
    const final = (await get('/api/runners/tasks')).find(t => t.id === task.id);
    assert.equal(final.status, 'success');
    assert.equal(final.result, 'current result');
    const empty = await call('POST', '/runners/tasks/claim', claim, [204]);
    assert.equal(empty.headers.get('X-Runner-Task-Protocol'), '2');

    const canceled = await post('/api/runners/tasks', { provider: 'fencing-test', prompt: 'cancel expiry', runnerId: runner.runnerId, maxAttempts: 3 });
    const leased = (await call('POST', '/runners/tasks/claim', claim)).data.task;
    await post(`/api/runners/tasks/${canceled.id}/cancel`, {});
    await call('POST', `/runners/tasks/${canceled.id}/complete`, { attempt: leased.attempt, status: 'success' }, [409]);
    expireLease(canceled.id);
    await waitFor('canceled expiry terminates without retry', async () => (await get('/api/runners/tasks')).find(t => t.id === canceled.id)?.status === 'canceled', { timeoutMs: 35_000, intervalMs: 250 });
    await call('POST', '/runners/tasks/claim', claim, [204]);
  } finally {
    for (const token of sessions) {
      await api('POST', '/runners/deregister', {}, { auth: false, headers: { Authorization: `Bearer ${token}` }, expect: [200, 401] });
    }
  }
});


test('manual MySQL migration retains legacy attempt zero and allows repeated sequence per attempt', {
  skip: !enabled && 'requires an explicitly named disposable local fencing MySQL container',
}, () => {
  const table = 'e2e_runner_events_migration_probe';
  const sql = input => execFileSync('docker', ['exec', '-i', container, 'mysql', '-uroot', '-proot', '--batch', '--skip-column-names', 'agentflow'], {
    input, encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'],
  });
  try {
    sql(`CREATE TABLE ${table} (task_id VARCHAR(64) NOT NULL, seq BIGINT NOT NULL, data TEXT, PRIMARY KEY(task_id,seq)); INSERT INTO ${table} VALUES ('legacy',1,'original');`);
    const migration = readFileSync(new URL('../../migrations/028_runner_task_attempt_events.sql', import.meta.url), 'utf8');
    sql(migration.replaceAll('runner_task_events', table));
    sql(`INSERT INTO ${table}(task_id,attempt,seq,data) VALUES ('legacy',1,1,'one'),('legacy',2,1,'two');`);
    assert.equal(sql(`SELECT task_id,attempt,seq,data FROM ${table} ORDER BY attempt,seq;`).trim(), 'legacy\t0\t1\toriginal\nlegacy\t1\t1\tone\nlegacy\t2\t1\ttwo');
    assert.equal(sql(`SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA='agentflow' AND TABLE_NAME='${table}' AND INDEX_NAME='PRIMARY';`).trim(), 'task_id,attempt,seq');
  } finally {
    sql(`DROP TABLE IF EXISTS ${table};`);
  }
});
