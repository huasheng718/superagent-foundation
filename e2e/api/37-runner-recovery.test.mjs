// Real native Runner faults against a disposable control plane. No lease clock
// is edited: the only SQL setup change raises the queued Script retry budget.
// The proxy forwards actual HTTP/WS bytes or closes actual sockets; it never
// fabricates a claim, renewal, execution or completion response.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer, request } from 'node:http';
import { connect } from 'node:net';
import { spawn, execFileSync } from 'node:child_process';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { once } from 'node:events';
import { createHash } from 'node:crypto';
import { api, get, post, put, list, ensureProject, waitFor, waitRun, cleanup } from '../lib/api.mjs';
import { cfg, uid } from '../lib/env.mjs';
import { createWorkflow, script, startRun } from '../lib/fixtures.mjs';

const container = process.env.AGENTFLOW_E2E_FENCING_MYSQL_CONTAINER || '';
const enabled = /^superagent-p0-fencing-[a-z0-9-]+$/.test(container)
  && ['127.0.0.1', 'localhost'].includes(new URL(cfg.base).hostname);
const options = { skip: !enabled && 'requires explicitly named disposable local fencing MySQL', timeout: 260_000 };
const testSourceSHA256 = createHash('sha256').update(readFileSync(new URL(import.meta.url))).digest('hex');
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const tickCount = dir => existsSync(join(dir,'ticks')) ? readFileSync(join(dir,'ticks'),'utf8').trim().split('\n').length : 0;
const sh = value => `'${value.replaceAll("'", "'\\''")}'`;
const alive = pid => {
  try { process.kill(pid, 0); } catch (err) { if (err.code === 'ESRCH') return false; throw err; }
  try { return !execFileSync('ps', ['-p', String(pid), '-o', 'stat='], { encoding: 'utf8' }).trim().startsWith('Z'); } catch { return false; }
};

class FaultProxy {
  online = true;
  holdClaims = true;
  held = [];
  sockets = new Set();
  trace = [];
  constructor(base) {
    this.upstream = new URL(base);
    assert.equal(this.upstream.protocol, 'http:', 'local raw fault proxy requires HTTP');
    this.server = createServer(async (req, res) => {
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const body = Buffer.concat(chunks);
      const forward = () => this.forward(req, res, body);
      if (!this.online) { req.socket.destroy(); return; }
      if (this.holdClaims && req.url === '/runners/tasks/claim') { this.held.push(forward); return; }
      forward();
    });
    this.server.on('connection', socket => this.track(socket));
    this.server.on('upgrade', (req, socket, head) => {
      if (!this.online) { socket.destroy(); return; }
      const upstream = connect(Number(this.upstream.port || 80), this.upstream.hostname);
      this.track(upstream);
      upstream.on('error', () => socket.destroy());
      socket.on('error', () => upstream.destroy());
      upstream.on('connect', () => {
        upstream.write(`${req.method} ${req.url} HTTP/${req.httpVersion}\r\n${Object.entries(req.headers).map(([key, value]) => `${key}: ${value}`).join('\r\n')}\r\n\r\n`);
        if (head.length) upstream.write(head);
        socket.pipe(upstream); upstream.pipe(socket);
      });
    });
  }
  track(socket) { this.sockets.add(socket); socket.on('close', () => this.sockets.delete(socket)); }
  forward(req, res, body) {
    if (!this.online || res.destroyed) { req.socket.destroy(); return; }
    const row = { at: new Date().toISOString(), method: req.method, path: req.url };
    if (/\/tasks\//.test(req.url)) {
      try { row.body = JSON.parse(body.toString()); } catch { /* GET has no body */ }
      row.protocol = req.headers['x-runner-task-protocol'];
      this.trace.push(row);
    }
    const upstream = request(new URL(req.url, this.upstream), { method: req.method, headers: req.headers, agent: false }, response => {
      row.status = response.statusCode;
      const bytes = [];
      response.on('data', chunk => bytes.push(chunk));
      response.on('end', () => {
        if (/\/tasks\//.test(req.url)) {
          try {
            const data = JSON.parse(Buffer.concat(bytes).toString());
            row.response = data.task ? { task: { id: data.task.id, attempt: data.task.attempt, status: data.task.status, leaseUntil: data.task.leaseUntil } } : data;
          } catch { /* empty 204 */ }
        }
      });
      res.writeHead(response.statusCode, response.headers); response.pipe(res);
    });
    upstream.on('socket', socket => this.track(socket));
    upstream.on('error', err => { row.transportError = err.code; res.destroy(); });
    res.on('close', () => upstream.destroy());
    upstream.end(body);
  }
  async listen() { this.server.listen(0, '127.0.0.1'); await once(this.server, 'listening'); this.url = `http://127.0.0.1:${this.server.address().port}`; }
  releaseClaims() { this.holdClaims = false; for (const forward of this.held.splice(0)) forward(); }
  disconnect() { this.online = false; this.trace.push({ at: new Date().toISOString(), fault: 'all proxy sockets destroyed; new connections refused' }); for (const socket of this.sockets) socket.destroy(); this.held = []; }
  reconnect() { this.online = true; this.trace.push({ at: new Date().toISOString(), fault: 'proxy transport restored' }); }
  async close() { this.disconnect(); this.server.closeAllConnections(); await new Promise(resolve => this.server.close(resolve)); }
}

async function experiment(t, label) {
  await ensureProject();
  const dir = mkdtempSync(join(tmpdir(), `superagent-recovery-${label}-`));
  const evidenceDir = process.env.AGENTFLOW_E2E_RECOVERY_EVIDENCE_DIR || dir;
  mkdirSync(evidenceDir, { recursive: true });
  const facts = { label, testSourceSHA256, node: process.version, startedAt: new Date().toISOString(), host: { os: process.platform, arch: process.arch }, leaseClockEdited: false, processes: [] };
  const proxy = new FaultProxy(cfg.base); await proxy.listen();
  const previousDefault = (await get('/api/runners/default')).runnerId || '';
  let proc; let runnerId = ''; let session = ''; let code = ''; let logs = '';
  const statePath = join(dir, 'runner-state.json');
  const binary = join(dir, 'agentflow-runner');
  const arch = { x64: 'amd64', arm64: 'arm64' }[process.arch]; assert.ok(arch);
  const response = await fetch(`${cfg.base}/runners/agentflow-runner?os=${process.platform}&arch=${arch}`, { signal: AbortSignal.timeout(120_000) });
  assert.equal(response.status, 200, 'native Runner download');
  writeFileSync(binary, Buffer.from(await response.arrayBuffer())); chmodSync(binary, 0o755);
  facts.runnerBinarySHA256 = createHash('sha256').update(readFileSync(binary)).digest('hex');
  const name = uid(`recovery-${label}`);
  const persist = () => {
    writeFileSync(join(evidenceDir, `${label}-facts.json`), JSON.stringify(facts, null, 2));
    writeFileSync(join(evidenceDir, `${label}-http.json`), JSON.stringify(proxy.trace, null, 2));
    writeFileSync(join(evidenceDir, `${label}-runner.log`), logs);
  };
  const start = () => {
    proc = spawn(binary, ['--server', proxy.url, '--name', name, '--state', statePath,
      '--workspace', join(dir, 'workspace'), '--heartbeat', '2s', '--long-poll', '40s', '--auto-update=false'], { cwd: dir, env: { ...process.env, AGENTFLOW_RUNNER_BOOTSTRAP_TOKEN: code, AGENTFLOW_RUNNER_CONCURRENCY: '1', AGENTFLOW_MANAGED_CLI_ROOT: '' }, stdio: ['ignore', 'pipe', 'pipe'] });
    facts.processes.push({ pid: proc.pid, startedAt: new Date().toISOString() });
    proc.stdout.on('data', data => { logs += data; }); proc.stderr.on('data', data => { logs += data; });
    proc.on('error', err => { logs += `\nspawn error ${err.message}`; });
    proc.on('exit', (status, signal) => { logs += `\nrunner exit ${status} signal ${signal}\n`; });
    return proc;
  };
  t.after(async () => {
    proxy.reconnect();
    // Only Runner and child PIDs captured from this experiment are touched.
    proc?.kill('SIGTERM'); await delay(1200); if (proc?.exitCode === null) proc.kill('SIGKILL');
    for (const file of ['shell.pid', 'child.pid']) {
      const path = join(dir, file);
      if (existsSync(path)) {
        const pid = Number(readFileSync(path, 'utf8').trim());
        if (Number.isInteger(pid) && pid > 1 && alive(pid)) {
          try {
            const args = execFileSync('ps', ['-p', String(pid), '-o', 'command='], { encoding: 'utf8' });
            if (args.includes(dir)) { process.kill(pid, 'SIGKILL'); facts.cleanupKilledPIDs ||= []; facts.cleanupKilledPIDs.push(pid); }
          } catch (err) { if (alive(pid)) throw err; }
        }
      }
    }
    await put('/api/runners/default', { runnerId: previousDefault }, { expect: [200, 404] }).catch(() => put('/api/runners/default', { runnerId: '' }));
    await cleanup();
    if (session) await api('POST', '/runners/deregister', {}, { auth: false, headers: { Authorization: `Bearer ${session}` }, expect: [200, 401] }).catch(() => {});
    await proxy.close();
    facts.finishedAt = new Date().toISOString(); persist();
    // Never preserve the session file/binary; retain redacted evidence only.
    rmSync(statePath, { force: true }); rmSync(binary, { force: true });
    t.diagnostic(`redacted recovery evidence: ${evidenceDir}/${label}-{facts.json,http.json,runner.log}`);
  });
  ({ code } = await post('/api/runners/bind-codes', {})); start();
  const runner = await waitFor('native fault Runner online', async () => list(await get('/api/runners')).find(r => r.name === name && r.status === 'online'), { timeoutMs: 30_000, intervalMs: 250 });
  runnerId = runner.id; facts.runnerId = runnerId;
  session = JSON.parse(readFileSync(statePath, 'utf8')).sessionToken;
  await put('/api/runners/default', { runnerId });
  const scriptBody = `
if mkdir ${sh(join(dir, 'first-execution'))} 2>/dev/null; then
  echo $$ > ${sh(join(dir, 'shell.pid'))}
  echo FIRST_EXECUTION
  sh -c 'echo $$ > "$1/child.pid"; while :; do date +%s >> "$1/ticks"; sleep 1; done' recovery-child ${sh(dir)} &
  child=$!
  while kill -0 "$child" 2>/dev/null; do echo FIRST_TICK; sleep 1; done
  wait "$child"
else
  echo SECOND_EXECUTION
  echo $$ > ${sh(join(dir, 'second.pid'))}
  while test ! -f ${sh(join(dir, 'allow-finish'))}; do sleep 1; done
  echo recovered > recovered.txt
  echo SECOND_FINISHED
fi`;
  const wf = await createWorkflow(`recovery-${label}`, [script('fault', scriptBody, { artifactOutputs: [{ name: 'recovered', path: 'recovered.txt', mediaType: 'text/plain', required: true }] })], { sandboxProfileId: 'local' });
  const run = await startRun(wf, `real ${label} fault`); facts.runId = run.id;
  const task = await waitFor('queued Script task before claim', async () => list(await get('/api/runners/tasks')).find(task => task.runId === run.id && task.nodeId === 'fault' && task.status === 'queued'), { timeoutMs: 30_000, intervalMs: 250 });
  assert.match(task.id, /^[a-zA-Z0-9_-]+$/);
  execFileSync('docker', ['exec', '-i', container, 'mysql', '-uroot', '-proot', 'agentflow'], {
    input: `UPDATE runner_tasks SET max_attempts=3 WHERE id='${task.id}' AND status='queued' AND attempt=0;`, stdio: ['pipe', 'pipe', 'pipe'],
  });
  facts.setupOnlyRetryBudget = { taskId: task.id, from: 1, to: 3, when: 'before first claim' };
  proxy.releaseClaims();
  const readTask = async () => list(await get('/api/runners/tasks')).find(item => item.id === task.id);
  const first = await waitFor('first Script attempt running with real descendants', async () => {
    const current = await readTask(); return current?.status === 'running' && existsSync(join(dir, 'child.pid')) ? current : null;
  }, { timeoutMs: 30_000, intervalMs: 250 });
  await waitFor('real first attempt events persisted', async () => (await api('GET', `/api/runners/tasks/${task.id}/events`, undefined, { auth: false, headers: { Authorization: `Bearer ${session}` } })).some(event => event.attempt === first.attempt && event.data.includes('FIRST_EXECUTION')), { timeoutMs: 10_000, intervalMs: 250 });
  const shellPID = Number(readFileSync(join(dir, 'shell.pid'), 'utf8'));
  const childPID = Number(readFileSync(join(dir, 'child.pid'), 'utf8'));
  assert.ok(alive(shellPID) && alive(childPID), 'actual script and descendant are live');
  facts.first = { taskId: task.id, attempt: first.attempt, leaseUntil: first.leaseUntil, shellPID, childPID };
  const call = async (suffix, body, expect = [409]) => {
    const method = suffix.startsWith('control') ? 'GET' : 'POST';
    const response = await api(method, `/runners/tasks/${task.id}/${suffix}`, body, { auth: false, raw: true, expect, headers: { Authorization: `Bearer ${session}` } });
    proxy.trace.push({ at: new Date().toISOString(), replay: true, method, path: suffix, body, status: response.status, response: response.data });
    return response;
  };
  const reclaim = async () => {
    const second = await waitFor('same Runner reclaims next real attempt', async () => {
      const current = await readTask(); return current?.status === 'running' && current.attempt === first.attempt + 1 ? current : null;
    }, { timeoutMs: 45_000, intervalMs: 250 });
    assert.equal(second.leaseOwner, runnerId); facts.second = { taskId: task.id, attempt: second.attempt, leaseUntil: second.leaseUntil };
    for (const [suffix, extra] of [['start', {}], ['lease/renew', {}], ['events', { events: [{ seq: 1, type: 'stdout', data: 'late old output' }] }], ['complete', { status: 'success', result: 'late old final' }]]) {
      await call(suffix, { attempt: first.attempt, ...extra });
    }
    await call(`control?attempt=${first.attempt}`, undefined);
    const history = await waitFor('new attempt durable event sequence after start', async () => {
      const events=await api('GET', `/api/runners/tasks/${task.id}/events`, undefined, { auth: false, headers: { Authorization: `Bearer ${session}` } });
      return events.some(event=>event.attempt===second.attempt&&event.seq===1) ? events : null;
    }, {timeoutMs:10_000,intervalMs:250});
    assert.ok(history.some(event => event.attempt === first.attempt && event.seq === 1));
    assert.ok(history.some(event => event.attempt === second.attempt && event.seq === 1));
    assert.ok(!history.some(event => event.data === 'late old output'));
    // Replay an actual acknowledged batch after reconnect, in reverse order.
    const batch = proxy.trace.find(row => row.path === `/runners/tasks/${task.id}/events` && row.body?.attempt === second.attempt && row.status === 200 && row.body.events?.length)?.body;
    assert.ok(batch, 'new attempt uploaded a real batch');
    const duplicate = await call('events', { ...batch, events: [...batch.events].reverse() }, [200]);
    assert.equal(duplicate.data.accepted, 0);
    facts.history = history.map(({ attempt, seq, type, data }) => ({ attempt, seq, type, data }));
    writeFileSync(join(dir, 'allow-finish'), 'yes');
    const final = await waitRun(run.id, run => run.status === 'succeeded', { timeoutMs: 30_000, label: 'recovered Script succeeds' });
    const artifact = final.artifacts?.find(item => item.name === 'recovered'); assert.ok(artifact);
    const content = await api('GET', `/api/runs/${run.id}/artifacts/${encodeURIComponent(artifact.id)}`, undefined, { raw: true });
    assert.equal(content.text, 'recovered\n');
    facts.finalTask = await readTask(); assert.equal(facts.finalTask.attempt, second.attempt); assert.equal(facts.finalTask.status, 'success');
    assert.notEqual(facts.finalTask.result, 'late old final'); persist();
  };
  return { dir, facts, proxy, first, shellPID, childPID, readTask, reclaim, start, currentProcess: () => proc, persist, readEvents: () => api('GET', `/api/runners/tasks/${task.id}/events`, undefined, { auth:false,headers:{Authorization:`Bearer ${session}`} }) };
}

test('native Runner: real network loss expires lease, stops descendants and fences reconnect', options, async t => {
  const e = await experiment(t, 'network');
  // A short actual outage must flush queued native output on the same lease.
  const before = await e.readEvents();
  e.facts.shortNetworkCutAt = new Date().toISOString(); e.proxy.disconnect();
  await delay(4_000); assert.ok(alive(e.shellPID) && alive(e.childPID));
  e.proxy.reconnect();
  const recovered = await waitFor('offline native events replay after short reconnect', async () => {
    const events = await e.readEvents(); return events.length >= before.length + 3 ? events : null;
  }, {timeoutMs:15_000,intervalMs:250});
  assert.equal((await e.readTask()).attempt,e.first.attempt,'short reconnect must not create a new lease generation');
  assert.deepEqual(recovered.map(event=>event.seq),Array.from({length:recovered.length},(_,i)=>i+1),'offline queue must replay contiguous event IDs without loss/duplicates');
  e.facts.shortReconnect = {at:new Date().toISOString(),attempt:e.first.attempt,beforeEvents:before.length,recoveredEvents:recovered.length};
  // Capture the last acknowledged lease after preparation and first event.
  const latest = await e.readTask(); const leaseDeadline = Date.parse(latest.leaseUntil);
  e.facts.networkCutAt = new Date().toISOString(); e.facts.ticksAtNetworkCut = tickCount(e.dir); e.facts.lastAcknowledgedLeaseUntil = latest.leaseUntil; e.proxy.disconnect(); e.persist();
  await waitFor('real lease deadline passes without renewals', async () => Date.now() >= leaseDeadline, { timeoutMs: 130_000, intervalMs: 500 });
  await waitFor('direct Script shell stops after lost lease', async () => !alive(e.shellPID), { timeoutMs: 15_000, intervalMs: 250 });
  e.facts.shellStoppedAt = new Date().toISOString();
  await waitFor('descendant termination grace after lost lease', async () => !alive(e.childPID), { timeoutMs: 15_000, intervalMs: 250 }).catch(() => {});
  e.facts.descendantAliveAfterLostLease = alive(e.childPID); e.facts.ticksAfterLeaseAndGrace = tickCount(e.dir); e.persist();
  await waitFor('real expired task requeued by production sweeper', async () => (await e.readTask())?.status === 'queued', { timeoutMs: 35_000, intervalMs: 250 });
  e.proxy.reconnect(); e.facts.reconnectedAt = new Date().toISOString();
  await e.reclaim();
  assert.equal(e.facts.descendantAliveAfterLostLease, false, 'lost lease left the real Script descendant running');
});

test('native Runner: actual SIGKILL and same-state restart recover fenced attempt', options, async t => {
  const e = await experiment(t, 'restart');
  const killed = e.currentProcess(); const exited = once(killed, 'exit');
  killed.kill('SIGKILL'); const [code, signal] = await exited;
  assert.equal(signal, 'SIGKILL'); e.facts.killedAt = new Date().toISOString(); e.facts.killedRunnerPID = killed.pid;
  await delay(300); e.facts.descendantAliveImmediatelyAfterSIGKILL = alive(e.childPID); e.facts.ticksAfterSIGKILL = tickCount(e.dir);
  const latest = await e.readTask(); const leaseDeadline = Date.parse(latest.leaseUntil);
  e.facts.lastAcknowledgedLeaseUntil = latest.leaseUntil; e.persist();
  await waitFor('real killed Runner lease expires', async () => Date.now() >= leaseDeadline, { timeoutMs: 130_000, intervalMs: 500 });
  await waitFor('killed attempt production sweep', async () => (await e.readTask())?.status === 'queued', { timeoutMs: 35_000, intervalMs: 250 });
  e.facts.descendantAliveAfterExpiry = alive(e.childPID); e.facts.ticksAfterExpiry = tickCount(e.dir); e.persist();
  e.start(); await e.reclaim();
  assert.equal(e.facts.descendantAliveAfterExpiry, false, 'SIGKILL orphan kept running beyond its authorization deadline');
});
