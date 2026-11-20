// Opt-in actual Codex execution through the native Runner. Credentials are
// supplied explicitly in a private file outside the checkout, never fixtures.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, writeFileSync, chmodSync, rmSync, existsSync, mkdirSync, statSync, readdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { api, get, post, put, list, cleanup, ensureProject, waitFor, waitRun, nodeStatus } from '../lib/api.mjs';
import { cfg, uid } from '../lib/env.mjs';
import { createAgent, createWorkflow, agentNode, script, approval, startRun } from '../lib/fixtures.mjs';

const credentialFile = process.env.AGENTFLOW_E2E_CODEX_CREDENTIAL_FILE || '';
const enabled = Boolean(credentialFile);
const cancelOnly = process.env.AGENTFLOW_E2E_CODEX_CASE === 'cancel';
let dir, proc, runnerId, previousDefault, credentials, runnerLog = '';
const activeRuns = [];
const evidenceDir = process.env.AGENTFLOW_E2E_EVIDENCE_DIR || '';
const record = (name, value) => {
  if (evidenceDir) { mkdirSync(evidenceDir, { recursive: true }); writeFileSync(join(evidenceDir, name + '.json'), JSON.stringify(value, (key, item) => /(?:apiKey|authToken|sessionToken|gitToken|credential|password|secret)/i.test(key) ? '[REDACTED]' : item, 2).replaceAll(credentials?.key || '__no_key__', '[REDACTED]').replaceAll(credentials?.baseUrl || '__no_endpoint__', '[configured provider endpoint]') + '\n'); }
};

before(async () => {
  if (!enabled) return;
  assert.ok(['127.0.0.1', 'localhost'].includes(new URL(cfg.base).hostname), 'credentials require a disposable local platform');
  assert.equal(statSync(credentialFile).mode & 0o077, 0, 'credential file must be owner-only');
  credentials = JSON.parse(readFileSync(credentialFile, 'utf8'));
  assert.ok(credentials.key && credentials.baseUrl && credentials.model);
  await ensureProject();
  const user = list(await get('/api/users')).find(u => u.loginName === 'e2e-local');
  assert.ok(user, 'use e2e/local.sh seeded disposable identity');
  await put(`/api/users/${user.id}/credentials`, { codexApiKey: credentials.key, codexBaseUrl: credentials.baseUrl });
  previousDefault = (await get('/api/runners/default')).runnerId || '';
  dir = mkdtempSync(join(tmpdir(), 'superagent-codex-baseline-'));
  const res = await fetch(`${cfg.base}/runners/agentflow-runner?os=${process.platform}&arch=${process.arch}`, { signal: AbortSignal.timeout(120_000) });
  assert.equal(res.status, 200);
  const binary = join(dir, 'agentflow-runner');
  writeFileSync(binary, Buffer.from(await res.arrayBuffer())); chmodSync(binary, 0o700);
  const { code } = await post('/api/runners/bind-codes', {});
  const name = uid('codex-native');
  proc = spawn(binary, ['--server', cfg.base, '--name', name, '--state', join(dir, 'state.json'), '--workspace', join(dir, 'workspace'), '--heartbeat', '5s', '--auto-update=false'], {
    env: { ...process.env, AGENTFLOW_RUNNER_BOOTSTRAP_TOKEN: code, AGENTFLOW_BROWSER: 'off' }, stdio: ['ignore', 'pipe', 'pipe'],
  });
  for (const stream of [proc.stdout, proc.stderr]) stream.on('data', d => { runnerLog += d; });
  const runner = await waitFor('native Codex runtime', async () => {
    const r = list(await get('/api/runners')).find(r => r.name === name);
    return r?.status === 'online' && r.runtimes?.some(rt => rt.provider === 'codex' && rt.status === 'online') ? r : null;
  }, { timeoutMs: 90_000 });
  runnerId = runner.id;
  await put('/api/runners/runtimes', { runnerId, provider: 'codex', authMode: 'personal' });
  const configured = list(await get('/api/runners')).find(r => r.id === runnerId);
  assert.equal(configured.runtimes.find(r => r.provider === 'codex').authMode, 'personal');
  await put('/api/runners/default', { runnerId });
  record('codex-runner-environment', { runnerId, platform: process.platform, arch: process.arch, version: runner.runnerVersion, runtime: runner.runtimes.find(r => r.provider === 'codex'), model: credentials.model, credentialConfigured: true });
});

after(async () => {
  if (!enabled) return;
  // Preserve the actual failed state before cleanup cancels and deletes it.
  for (const id of activeRuns) {
    try {
      const run = await get(`/api/runs/${id}`);
      const tasks = list(await get('/api/runners/tasks')).filter(t => t.runId === id);
      record('codex-final-snapshot-' + id, { run, tasks });
      const events = [];
      const session = JSON.parse(readFileSync(join(dir, 'state.json'), 'utf8'));
      for (const task of tasks) {
        try { events.push({ taskId: task.id, response: await get(`/api/runners/tasks/${task.id}/events`, { auth: false, headers: { Authorization: `Bearer ${session.sessionToken}` } }) }); }
        catch (error) { events.push({ taskId: task.id, error: error.message }); }
      }
      record('codex-events-' + id, events);
    } catch (error) { record('codex-snapshot-error', { message: error.message }); }
  }
  if (dir && evidenceDir) {
    const traces = [];
    const walk = path => {
      for (const entry of readdirSync(path, { withFileTypes: true })) {
        const child = join(path, entry.name);
        if (entry.isDirectory()) walk(child);
        else if (entry.isFile() && child.includes('/.codex/sessions/') && child.endsWith('.jsonl') && statSync(child).size < 8_000_000) traces.push({ path: child.slice(dir.length), jsonl: readFileSync(child, 'utf8') });
      }
    };
    if (existsSync(dir)) walk(dir);
    record('codex-session-traces', traces);
  }
  await cleanup();
  if (previousDefault !== undefined) await put('/api/runners/default', { runnerId: previousDefault }, { expect: [200, 404] }).catch(() => {});
  const state = dir && existsSync(join(dir, 'state.json')) ? JSON.parse(readFileSync(join(dir, 'state.json'), 'utf8')) : null;
  proc?.kill('SIGTERM');
  if (proc && proc.exitCode === null) await Promise.race([new Promise(resolve => proc.once('exit', resolve)), new Promise(resolve => setTimeout(resolve, 15_000))]);
  if (proc && proc.exitCode === null) proc.kill('SIGKILL');
  if (state?.sessionToken) await api('POST', '/runners/deregister', {}, { auth: false, headers: { Authorization: `Bearer ${state.sessionToken}` }, expect: [200, 401] }).catch(() => {});
  if (evidenceDir && credentials) writeFileSync(join(evidenceDir, 'codex-native-runner.log'), runnerLog.replaceAll(credentials.key, '[REDACTED]').replaceAll(credentials.baseUrl, '[configured provider endpoint]'));
  if (dir) rmSync(dir, { recursive: true, force: true });
});

const artifactText = async (run, name) => {
  const a = run.artifacts?.find(a => a.name === name);
  assert.ok(a, `missing ${name}`);
  return (await api('GET', `/api/runs/${run.id}/artifacts/${encodeURIComponent(a.id)}`, undefined, { raw: true })).text;
};

test('actual Codex fixes a disposable Git repository, tests a commit and stops at a human gate', { skip: !enabled || cancelOnly, timeout: 360_000 }, async () => {
  const agentId = await createAgent('codex-fix', 'Only work in this disposable repository. Read clamp.py and test_clamp.py, implement clamp(value, lower, upper) correctly, run python3 -m unittest test_clamp -v, and write implementation.md explaining actual changes and test results. Do not modify tests, execute Git, use network, install packages, or access files outside the workspace.', { executorId: 'codex', model: credentials.model, loop: { enabled: false } });
  const fixture = [
    'set -eu', 'git init -q -b main', 'git config user.name P0-Test', 'git config user.email p0@example.invalid',
    "printf 'def clamp(value, lower, upper):\\n    return value\\n' > clamp.py",
    "cat > test_clamp.py <<'PY'", 'import unittest', 'from clamp import clamp', 'class ClampTests(unittest.TestCase):',
    '    def test_lower(self): self.assertEqual(0, clamp(-2, 0, 10))', '    def test_inside(self): self.assertEqual(4, clamp(4, 0, 10))', '    def test_upper(self): self.assertEqual(10, clamp(12, 0, 10))', 'PY',
    'git add clamp.py test_clamp.py', 'git commit -qm baseline', 'git rev-parse HEAD > base-sha.txt',
  ].join('\n');
  const output = (name, path, mediaType = 'text/plain') => ({ name, path, mediaType, required: true });
  const workflow = await createWorkflow('codex-baseline', [
    script('fixture', fixture),
    agentNode('fix', agentId, { executorId: 'codex', artifactOutputs: [output('code', 'clamp.py'), output('implementation', 'implementation.md', 'text/markdown')] }),
    script('verify', 'set -eu\npython3 -m unittest test_clamp -v > verification.txt 2>&1\ngit diff -- clamp.py > change.patch\ntest -s change.patch\ngit add clamp.py implementation.md\ngit commit -qm "P0 actual Codex change"\ngit rev-parse HEAD > result-sha.txt\ngit show --stat HEAD > commit.txt\nshasum -a 256 clamp.py > code.sha256', { artifactOutputs: [output('tests', 'verification.txt'), output('patch', 'change.patch'), output('base-sha', 'base-sha.txt'), output('result-sha', 'result-sha.txt'), output('commit', 'commit.txt')] }),
    approval('human', 'fix'),
    script('confirm', 'set -eu\npython3 -m unittest test_clamp\nshasum -a 256 -c code.sha256'),
  ], { sandboxProfileId: 'local' });
  const { id } = await startRun(workflow, 'P0 actual native Codex implementation and gate acceptance test');
  activeRuns.push(id);
  const waiting = await waitRun(id, r => nodeStatus(r, 'human') === 'waiting_approval', { timeoutMs: 260_000, label: 'actual Codex awaiting approval' });
  assert.equal(waiting.executionContext.executionMode, 'runner'); assert.equal(waiting.executionContext.executionRunnerId, runnerId);
  for (const node of ['fixture', 'fix', 'verify']) assert.equal(nodeStatus(waiting, node), 'succeeded');
  assert.equal(nodeStatus(waiting, 'confirm'), 'pending', 'no completion before approval');
  const base = (await artifactText(waiting, 'base-sha')).trim(), result = (await artifactText(waiting, 'result-sha')).trim();
  assert.match(base, /^[a-f0-9]{40}$/); assert.match(result, /^[a-f0-9]{40}$/); assert.notEqual(base, result);
  assert.match(await artifactText(waiting, 'tests'), /Ran 3 tests[\s\S]*OK/);
  assert.match(await artifactText(waiting, 'patch'), /diff --git/);
  const tasks = list(await get('/api/runners/tasks')).filter(t => t.runId === id);
  assert.ok(tasks.filter(t => t.provider === 'codex').every(t => t.invocation.credentialSource === 'personal'), 'actual task must use isolated personal credential binding');
  assert.ok(tasks.some(t => t.provider === 'codex' && t.attempt > 0 && t.status === 'success'));
  const state = JSON.parse(readFileSync(join(dir, 'state.json'), 'utf8'));
  await api('POST', `/api/runs/${id}/approve`, { nodeId: 'human' }, { auth: false, headers: { Authorization: `Bearer ${state.sessionToken}` }, expect: [401, 403] });
  await post(`/api/runs/${id}/approve`, { nodeId: 'human', context: 'P0 automated authorization test: actual Codex diff, three tests and fixed commit verified; not a human business acceptance claim.' });
  const done = await waitRun(id, r => r.status === 'succeeded', { label: 'approved baseline complete' });
  record('codex-baseline-result', { runId: id, runnerId, baseSHA: base, resultSHA: result, tasks: tasks.map(t => ({ id: t.id, nodeId: t.nodeId, provider: t.provider, attempt: t.attempt, status: t.status })), nodeStates: Object.fromEntries(Object.entries(done.nodes).map(([id, n]) => [id, { status: n.status, attempt: n.attempt, sessionId: n.sessionId, usage: n.usage }])), artifacts: done.artifacts.map(a => ({ id: a.id, name: a.name, sha256: a.sha256 })), approvalMode: 'API exercised authenticated user vs denied Runner; no human business acceptance claimed' });
});

const livePID = pid => {
  try { return !execFileSync('ps', ['-o', 'stat=', '-p', String(pid)], { encoding: 'utf8' }).trim().startsWith('Z') && Boolean(execFileSync('ps', ['-o', 'stat=', '-p', String(pid)], { encoding: 'utf8' }).trim()); }
  catch { return false; }
};

test('cancelling an actual Codex invocation stops its running tool process', { skip: !enabled, timeout: 150_000 }, async () => {
  const command = `python3 -c "import os,time; open('model-tool.pid','w').write(str(os.getpid())); print('P0_MODEL_TOOL_STARTED',flush=True); time.sleep(180)"`;
  const agentId = await createAgent('codex-cancel', `Execute exactly this foreground command in the workspace: ${command}. Wait for it to finish. Do not background it, change the command, access other files, or run any other tool before it. Only after it finishes, submit the normal node result.`, { executorId: 'codex', model: credentials.model, loop: { enabled: false } });
  const wf = await createWorkflow('codex-cancel', [agentNode('hold', agentId, { executorId: 'codex' })], { sandboxProfileId: 'local' });
  const { id } = await startRun(wf, 'P0 actual Codex tool cancellation');
  activeRuns.push(id);
  const path = join(dir, 'workspace', 'runs', id, 'workspace', 'model-tool.pid');
  const pid = await waitFor('actual model started foreground Python tool', async () => {
    if (!existsSync(path)) return null;
    const value = Number(readFileSync(path, 'utf8').trim());
    return Number.isInteger(value) && value > 1 && livePID(value) ? value : null;
  }, { timeoutMs: 90_000 });
  const beforeCancel = list(await get('/api/runners/tasks')).find(t => t.runId === id && t.provider === 'codex');
  assert.equal(beforeCancel?.invocation?.credentialSource, 'personal');
  assert.ok(['running', 'leased'].includes(beforeCancel.status));
  record('codex-cancellation-before', { runId: id, runnerId, pid, runnerPID: proc.pid, taskId: beforeCancel.id, attempt: beforeCancel.attempt, ps: execFileSync('ps', ['-o', 'pid=,ppid=,pgid=,stat=,comm=', '-p', String(pid)], { encoding: 'utf8' }) });
  await post(`/api/runs/${id}/cancel`, {});
  const canceled = await waitRun(id, r => r.status === 'cancelled', { timeoutMs: 30_000 });
  try {
    await waitFor('actual Codex tool process stopped', async () => !livePID(pid), { timeoutMs: 15_000, intervalMs: 200 });
  } catch (error) {
    record('codex-cancellation-orphan', { pid, runnerPID: proc.pid, ps: execFileSync('ps', ['-o', 'pid=,ppid=,pgid=,stat=,comm=', '-p', String(pid)], { encoding: 'utf8' }), error: error.message });
    // Clean only the exact process this case observed starting its marker.
    const args = execFileSync('ps', ['-o', 'args=', '-p', String(pid)], { encoding: 'utf8' });
    if (args.includes('model-tool.pid') && args.includes('time.sleep(180)')) process.kill(pid, 'SIGKILL');
    throw error;
  }
  const tasks = await waitFor('cancelled execution task terminal', async () => {
    const items = list(await get('/api/runners/tasks')).filter(t => t.runId === id && !t.action);
    return items.length && items.every(t => t.status === 'canceled' || t.status === 'failed') ? items : null;
  }, { timeoutMs: 30_000, intervalMs: 200 });
  assert.ok(tasks.every(t => t.status !== 'success' && !t.acceptedContributions?.length), 'cancelled code must not be accepted as success');
  record('codex-cancellation-result', { runId: id, runnerId, pid, processStopped: !livePID(pid), runStatus: canceled.status, tasks: tasks.map(t => ({ id: t.id, attempt: t.attempt, status: t.status })) });
});

test('actual Codex tool execution stops at the persisted node deadline', { skip: !enabled || cancelOnly, timeout: 150_000 }, async () => {
  const command = `python3 -c "import os,time; open('deadline-tool.pid','w').write(str(os.getpid())); print('P0_DEADLINE_TOOL_STARTED',flush=True); time.sleep(180)"`;
  const agentId = await createAgent('codex-deadline', `Execute exactly this foreground command in the workspace: ${command}. Wait for it to finish. Do not background it, change the command, access other files, or run any other tool before it. Only after it finishes, submit the normal node result.`, { executorId: 'codex', model: credentials.model, loop: { enabled: false } });
  const wf = await createWorkflow('codex-deadline', [agentNode('hold', agentId, { executorId: 'codex', maxDurationSec: 30 })], { sandboxProfileId: 'local' });
  const { id } = await startRun(wf, 'P0 actual Codex absolute deadline');
  activeRuns.push(id);
  const path = join(dir, 'workspace', 'runs', id, 'workspace', 'deadline-tool.pid');
  const pid = await waitFor('actual Codex deadline tool running', async () => {
    if (!existsSync(path)) return null;
    const value = Number(readFileSync(path, 'utf8').trim());
    return Number.isInteger(value) && value > 1 && livePID(value) ? value : null;
  }, { timeoutMs: 25_000, intervalMs: 200 });
  const task = list(await get('/api/runners/tasks')).find(t => t.runId === id && t.provider === 'codex');
  const deadline = task.executionDeadline;
  assert.ok(deadline && new Date(deadline).getTime() > Date.now(), 'deadline was frozen before tool execution');
  record('codex-deadline-before', { runId: id, taskId: task.id, attempt: task.attempt, deadline, pid });
  try {
    const failed = await waitRun(id, r => r.status === 'failed', { timeoutMs: 50_000, label: 'absolute deadline failure' });
    assert.match(failed.nodes.hold.error, /execution deadline exceeded/i);
    assert.equal(failed.nodes.hold.executionDeadline, deadline);
    await waitFor('deadline tool process stopped', async () => !livePID(pid), { timeoutMs: 15_000, intervalMs: 200 });
    const tasks = await waitFor('expired task terminal', async () => {
      const items = list(await get('/api/runners/tasks')).filter(t => t.runId === id && t.provider === 'codex');
      return items.every(t => t.status === 'failed' || t.status === 'canceled') ? items : null;
    }, { timeoutMs: 30_000, intervalMs: 200 });
    assert.equal(tasks.length, 1, 'execution deadline must not silently start another task');
    assert.equal(tasks[0].executionDeadline, deadline, 'renewal must not extend the deadline');
    assert.ok(!tasks[0].acceptedContributions?.length);
    record('codex-deadline-result', { runId: id, pid, processStopped: !livePID(pid), deadline, runStatus: failed.status, nodeError: failed.nodes.hold.error, tasks: tasks.map(t => ({ id: t.id, attempt: t.attempt, status: t.status, executionDeadline: t.executionDeadline })) });
  } finally {
    if (livePID(pid)) {
      const args = execFileSync('ps', ['-o', 'args=', '-p', String(pid)], { encoding: 'utf8' });
      if (args.includes('deadline-tool.pid') && args.includes('time.sleep(180)')) process.kill(pid, 'SIGKILL');
    }
  }
});
