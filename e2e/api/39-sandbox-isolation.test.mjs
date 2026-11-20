// A measurement harness, not an assertion that per-task sandboxes imply Git
// isolation. It completes both reuse modes before failing the isolation gate.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawn, execFileSync } from 'node:child_process';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { api, get, post, put, list, waitFor, ensureProject, cleanup, track } from '../lib/api.mjs';
import { cfg, uid } from '../lib/env.mjs';
import { createWorkflow, createRepository, dispatchWithRepository, trackRun, script, approval } from '../lib/fixtures.mjs';

const enabled = process.env.AGENTFLOW_E2E_ISOLATION_MYSQL_CONTAINER === 'superagent-p0-isolation'
  && ['127.0.0.1', 'localhost'].includes(new URL(cfg.base).hostname) && existsSync(cfg.repoCloneURL);
const workspaceOnly = process.env.AGENTFLOW_E2E_ISOLATION_PHASE === 'workspace';
const sh = value => `'${value.replaceAll("'", "'\\''")}'`;
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const git = (dir, ...args) => execFileSync('git', ['-C', dir, ...args], { encoding: 'utf8' }).trim();
const alive = pid => { try { process.kill(pid, 0); return true; } catch (error) { if (error.code === 'ESRCH') return false; throw error; } };

async function dispatchIsolated(workflowId, repositoryId, baseSHA, title) {
  const issue = await post('/api/issues', { projectId: cfg.projectId, title: `E2E ${title} ${cfg.runTag}`,
    description: 'disposable isolated attempt workspace validation', priority: 'medium', kind: 'requirement', status: 'todo', workflowId, repositoryIds: [repositoryId] });
  track(`issue ${issue.id}`, () => post(`/api/issues/${issue.id}/close`, {}, { expect: [200, 404, 409] }));
  await post(`/api/issues/${issue.id}/dispatch`, { workflowId, runnerWorkspaceLayout: 'attempt', workspaceBaseSHAs: { [repositoryId]: baseSHA } });
  const runs = await waitFor('isolated issue run', async () => {
    const items = list(await get(`/api/issues/${issue.id}/runs`)); return items.length ? items : null;
  }, { timeoutMs: 60_000 });
  const runId = runs[0].id || runs[0].runId; trackRun(runId); return runId;
}

// This helper executes as the Script's direct process. Grandchild termination
// is deliberately tested by 37-runner-recovery, not hidden in this experiment.
const helper = String.raw`import json, os, pathlib, subprocess, sys, time
root = pathlib.Path.cwd().resolve()
operation, sync_arg = sys.argv[1:3]
sync = pathlib.Path(sync_arg)
sync.mkdir(parents=True, exist_ok=True)
repositories = ([root] if (root / '.git').exists() else []) + [p.parent for p in root.glob('*/.git')]
assert len(repositories) == 1, str(repositories)
repo = repositories[0].resolve()
def git(*args):
    return subprocess.check_output(['git', '-C', str(repo), *args], text=True).strip()
def facts(node):
    common = pathlib.Path(git('rev-parse', '--git-common-dir'))
    if not common.is_absolute(): common = repo / common
    owner = root / '.agentflow-workspace-owner.json'
    return {'node': node, 'pid': os.getpid(), 'at': time.time(), 'workspace': str(root), 'repository': str(repo),
            'workspaceOwner': json.loads(owner.read_text()) if owner.exists() else None,
            'gitCommonDir': str(common.resolve()), 'gitDir': git('rev-parse', '--absolute-git-dir'),
            'branch': git('branch', '--show-current'), 'baseSHA': git('rev-parse', 'HEAD'),
            'worktrees': git('worktree', 'list', '--porcelain')}
def save(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2))
def wait(path, seconds=45):
    deadline = time.monotonic() + seconds
    while not path.exists():
        assert time.monotonic() < deadline, 'barrier timed out: ' + str(path)
        time.sleep(.1)
if operation == 'writer':
    node, scenario = sys.argv[3:5]
    own = 'frontend/a.txt' if node == 'a' else 'backend/b.txt'
    peer = 'backend/b.txt' if node == 'a' else 'frontend/a.txt'
    attempt = 1
    while True:
        try: (sync / (node + '-invocation-' + str(attempt))).mkdir(); break
        except FileExistsError: attempt += 1
    target = repo / own
    own_visible_before_write = target.exists()
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(node.upper() + '-invocation-' + str(attempt) + '\n')
    result = facts(node)
    (root / ('undeclared-' + node + '.txt')).write_text('private undeclared output')
    result.update({'invocation': attempt, 'ownPath': own, 'peerPath': peer, 'ownContent': target.read_text(), 'ownVisibleBeforeWrite': own_visible_before_write})
    save(sync / (node + '-started-' + str(attempt) + '.json'), result)
    (sync / (node + '-ready')).touch()
    wait(sync / (('b' if node == 'a' else 'a') + '-ready'))
    result['peerVisible'] = (repo / peer).exists()
    result['peerContent'] = (repo / peer).read_text() if result['peerVisible'] else None
    if scenario == 'cancel' and attempt == 1:
        deadline = time.monotonic() + 70
        while not (sync / ('release-' + node)).exists():
            assert time.monotonic() < deadline, 'unreleased cancellation experiment'
            (sync / (node + '-heartbeat')).write_text(str(time.time()))
            time.sleep(.1)
    else:
        time.sleep(1)
    result.update({'finishedAt': time.time(), 'resultSHA': git('rev-parse', 'HEAD'), 'gitStatus': git('status', '--porcelain'), 'peerVisibleAtEnd': (repo / peer).exists()})
    save(root / 'reports' / (node + '.json'), result)
    save(sync / (node + '-finished-' + str(attempt) + '.json'), result)
    print(json.dumps(result), flush=True)
elif operation == 'reader':
    result = facts('reader')
    result['declaredInputs'] = {}
    for node in ('a', 'b'):
        path = root / '.agentflow' / 'inputs' / node / (node + '.json')
        result['declaredInputs'][node] = {'path': str(path), 'content': json.loads(path.read_text())}
    result['aggregateFiles'] = {name: (repo / name).read_text() if (repo / name).exists() else None for name in ('frontend/a.txt', 'backend/b.txt')}
    result['undeclaredPeerFiles'] = {node: (root / ('undeclared-' + node + '.txt')).exists() for node in ('a', 'b')}
    result['resultSHA'] = git('rev-parse', 'HEAD')
    result['gitStatus'] = git('status', '--porcelain')
    save(root / 'reports' / 'reader.json', result)
    save(sync / 'reader.json', result)
    print(json.dumps(result), flush=True)
else:
    raise ValueError(operation)
`;

test(`P0-003 native Runner: ${workspaceOnly ? 'isolated assignment/checkout/artifact slice' : 'measure full Node Git isolation'}`, {
  skip: !enabled && 'requires disposable local superagent-p0-isolation MySQL and bare Git fixture', timeout: 420_000,
}, async t => {
  await ensureProject();
  const dir = mkdtempSync(join(tmpdir(), 'superagent-isolation-'));
  const evidence = process.env.AGENTFLOW_E2E_ISOLATION_EVIDENCE_DIR || dir;
  mkdirSync(evidence, { recursive: true });
  const helperPath = join(dir, 'isolation_probe.py'); writeFileSync(helperPath, helper);
  const binary = join(dir, 'agentflow-runner');
  const arch = { x64: 'amd64', arm64: 'arm64' }[process.arch]; assert.ok(arch);
  const response = await fetch(`${cfg.base}/runners/agentflow-runner?os=${process.platform}&arch=${arch}`, { signal: AbortSignal.timeout(120_000) });
  assert.equal(response.status, 200, 'native Runner download');
  writeFileSync(binary, Buffer.from(await response.arrayBuffer())); chmodSync(binary, 0o755);
  const previousDefault = (await get('/api/runners/default')).runnerId || '';
  const remotesBefore = git(cfg.repoCloneURL, 'for-each-ref', '--format=%(refname) %(objectname)', 'refs/heads');
  const report = { startedAt: new Date().toISOString(), sourceSHA: git(process.cwd(), 'rev-parse', 'HEAD'),
    sourcePatchSHA256: createHash('sha256').update(execFileSync('git', ['diff', 'HEAD'], { cwd: process.cwd() })).digest('hex'),
    harnessSHA256: createHash('sha256').update(readFileSync(new URL(import.meta.url))).digest('hex'),
    host: { platform: process.platform, arch: process.arch, modelUsed: false }, workspaceFixture: dir,
    boundaries: { driver: 'local', directProcessOnly: true, externalGitCredentialFencing: 'not tested', hostileSiblingFileAccess: 'not tested',
      phase: workspaceOnly ? 'workspace' : 'full', commitAggregation: workspaceOnly ? 'pending independent acceptance' : 'measured' }, modes: [], gatePassed: false, findings: [] };
  const logs = {};
  const runners = [];
  const persist = () => {
    writeFileSync(join(evidence, 'sandbox-isolation-facts.json'), JSON.stringify(report, null, 2));
    for (const [mode, log] of Object.entries(logs)) writeFileSync(join(evidence, `sandbox-isolation-reuse-${mode}-runner.log`), log);
  };
  t.after(async () => {
    await put('/api/runners/default', { runnerId: previousDefault }, { expect: [200, 404] }).catch(() => {});
    // Release only these disposable scripts before stopping their Runner.
    for (const mode of report.modes) for (const item of mode.scenarios || []) {
      if (item.syncDirectory) for (const node of ['a', 'b']) writeFileSync(join(item.syncDirectory, `release-${node}`), 'cleanup');
    }
    await cleanup();
    for (const runner of runners) {
      runner.proc.kill('SIGTERM'); await delay(700);
      if (runner.proc.exitCode === null && runner.proc.signalCode === null) runner.proc.kill('SIGKILL');
      if (existsSync(runner.state)) {
        const session = JSON.parse(readFileSync(runner.state, 'utf8')).sessionToken;
        await api('POST', '/runners/deregister', {}, { auth: false, headers: { Authorization: `Bearer ${session}` }, expect: [200, 401] }).catch(() => {});
        rmSync(runner.state, { force: true });
      }
    }
    report.finishedAt = new Date().toISOString(); persist();
    rmSync(binary, { force: true });
    t.diagnostic(`isolation evidence: ${join(evidence, 'sandbox-isolation-facts.json')}; fixture retained at ${dir}`);
  });
  for (const reuse of [true, false]) {
    const mode = { reuseRunSandbox: reuse, scenarios: [] }; report.modes.push(mode);
    const state = join(dir, `state-${reuse}.json`);
    const name = uid(`isolation-${reuse}`);
    const { code } = await post('/api/runners/bind-codes', {});
    const args = ['--server', cfg.base, '--token', code, '--name', name, '--state', state,
      '--workspace', join(dir, `workspace-${reuse}`), '--concurrency', '2', '--heartbeat', '2s', '--long-poll', '40s',
      '--auto-update=false', `--reuse-run-sandbox=${reuse}`];
    const proc = spawn(binary, args, { cwd: dir, stdio: ['ignore', 'pipe', 'pipe'] });
    logs[reuse] = '';
    proc.stdout.on('data', data => { logs[reuse] += data; }); proc.stderr.on('data', data => { logs[reuse] += data; });
    proc.on('error', error => { logs[reuse] += `\nspawn: ${error.message}`; });
    runners.push({ proc, state });
    const runner = await waitFor('native isolation Runner online', async () => list(await get('/api/runners')).find(r => r.name === name && r.status === 'online'), { timeoutMs: 30_000, intervalMs: 250 });
    mode.runnerId = runner.id; mode.runnerPID = proc.pid; mode.runnerVersion = runner.runnerVersion;
    mode.configuration = { reuseRunSandbox: reuse, concurrency: 2, autoUpdate: false, workspaceRoot: join(dir, `workspace-${reuse}`) };
    await put('/api/runners/default', { runnerId: runner.id });
    for (const scenario of ['parallel', 'cancel']) {
      const sync = join(dir, `sync-${reuse}-${scenario}`); mkdirSync(sync);
      const item = { scenario, syncDirectory: sync }; mode.scenarios.push(item); persist();
      const writer = node => script(node, `exec python3 ${sh(helperPath)} writer ${sh(sync)} ${node} ${scenario}`, {
        artifactOutputs: [{ name: node, path: `reports/${node}.json`, mediaType: 'application/json', required: true }],
      });
      const nodes = [script('start', 'echo PREPARED'), writer('a'), writer('b'),
        script('reader', `exec python3 ${sh(helperPath)} reader ${sh(sync)}`, {
          artifactOutputs: [{ name: 'reader', path: 'reports/reader.json', mediaType: 'application/json', required: true }],
        }), approval('hold', '')];
      const edges = [['start', 'a'], ['start', 'b'], ['a', 'reader'], ['b', 'reader'], ['reader', 'hold']]
        .map(([source, target]) => ({ id: source + '-' + target, source, target, when: 'success' }));
      const workflow = await createWorkflow(`isolation-${reuse}-${scenario}`, nodes, { sandboxProfileId: 'local', edges });
      const repository = await createRepository(`isolation-${reuse}-${scenario}`);
      item.pinnedBaseSHA = git(cfg.repoCloneURL, 'rev-parse', `refs/heads/${cfg.repoBranch}`);
      item.runId = workspaceOnly
        ? await dispatchIsolated(workflow, repository, item.pinnedBaseSHA, `isolation ${reuse} ${scenario}`)
        : await dispatchWithRepository(workflow, repository, `isolation ${reuse} ${scenario}`);
      const tasks = async () => list(await get('/api/runners/tasks')).filter(task => task.runId === item.runId);
      const readRun = () => get(`/api/runs/${item.runId}`);
      await waitFor('both writer processes started', async () => {
        if (existsSync(join(sync, 'a-started-1.json')) && existsSync(join(sync, 'b-started-1.json'))) return true;
        item.runBeforeWriters = await readRun(); item.tasksBeforeWriters = await tasks(); persist();
        if (['failed', 'cancelled', 'rejected'].includes(item.runBeforeWriters.status)) {
          throw new Error(`run ended before writer overlap: ${JSON.stringify(item.runBeforeWriters)}`);
        }
        return false;
      }, { timeoutMs: 45_000, intervalMs: 200 });
      item.started = Object.fromEntries(['a', 'b'].map(node => [node, JSON.parse(readFileSync(join(sync, `${node}-started-1.json`), 'utf8'))]));
      item.tasksAtOverlap = await tasks();
      item.actualOverlap = alive(item.started.a.pid) && alive(item.started.b.pid);
      assert.ok(item.actualOverlap, 'both real writer processes overlap');
      if (scenario === 'cancel') {
        const firstA = item.tasksAtOverlap.find(task => task.nodeId === 'a'); assert.ok(firstA);
        const bPath = join(item.started.b.repository, 'backend/b.txt');
        item.cancel = { taskId: firstA.id, attempt: firstA.attempt, aPID: item.started.a.pid, bPID: item.started.b.pid,
          bContentBefore: readFileSync(bPath, 'utf8'), requestedAt: new Date().toISOString() };
        await post(`/api/runners/tasks/${firstA.id}/cancel`, {});
        await waitFor('A task acknowledges cancellation', async () => (await tasks()).find(task => task.id === firstA.id)?.status === 'canceled', { timeoutMs: 30_000, intervalMs: 250 });
        const heartbeatBefore = readFileSync(join(sync, 'b-heartbeat'), 'utf8'); await delay(500);
        item.cancel.aStopped = !alive(item.started.a.pid);
        item.cancel.bAliveAfterCancel = alive(item.started.b.pid);
        item.cancel.bHeartbeatContinued = readFileSync(join(sync, 'b-heartbeat'), 'utf8') !== heartbeatBefore;
        item.cancel.bContentAfter = readFileSync(bPath, 'utf8');
        item.cancel.runAfterCancel = await readRun(); item.cancel.tasksAfterCancel = await tasks();
        writeFileSync(join(sync, 'release-b'), 'finish B');
        await waitFor('failed/canceled stage becomes retryable', async () => {
          const run = await readRun(); return ['failed', 'waiting', 'cancelled'].includes(run.status) ? run : null;
        }, { timeoutMs: 30_000, intervalMs: 250 });
        item.cancel.retryResponse = await post(`/api/runs/${item.runId}/retry`, { nodeId: 'a', reason: 'disposable P0 isolation cancellation/retry' }, { raw: true });
      }
      item.finalRun = await waitFor('declared handoff reaches approval hold', async () => {
        const run = await readRun();
        if (run.nodes?.hold?.status === 'waiting_approval') return run;
        if (['failed', 'cancelled', 'rejected'].includes(run.status)) throw new Error(`run ${run.id} ${run.status}: ${JSON.stringify(run.nodes)}`);
        return null;
      }, { timeoutMs: 60_000, intervalMs: 300 });
      item.finalTasks = await tasks();
      item.artifacts = {};
      for (const node of ['a', 'b', 'reader']) {
        const artifact = item.finalRun.artifacts.find(a => a.nodeId === node && a.name === node); assert.ok(artifact, `${node} declared artifact`);
        const response = await api('GET', `/api/runs/${item.runId}/artifacts/${encodeURIComponent(artifact.id)}`, undefined, { raw: true });
        item.artifacts[node] = { artifactId: artifact.id, sha256: artifact.sha256, content: JSON.parse(response.text) };
      }
      const a = item.artifacts.a.content, b = item.artifacts.b.content, reader = item.artifacts.reader.content;
      item.measurements = { workspaceSeparated: a.workspace !== b.workspace, repositorySeparated: a.repository !== b.repository,
        gitCommonDirSeparated: a.gitCommonDir !== b.gitCommonDir, gitDirSeparated: a.gitDir !== b.gitDir, branchSeparated: a.branch !== b.branch,
        aSawB: a.peerVisible, bSawA: b.peerVisible, headUnchanged: a.baseSHA === a.resultSHA && b.baseSHA === b.resultSHA,
        explicitHandoff: reader.declaredInputs.a.content.node === 'a' && reader.declaredInputs.b.content.node === 'b',
        aggregateFiles: reader.aggregateFiles, resultSHA: reader.resultSHA };
      assert.ok(item.measurements.explicitHandoff, 'reader receives both declared artifacts');
      if (workspaceOnly) {
        assert.equal(item.finalRun.executionContext.runnerWorkspaceLayout, 'attempt');
        for (const value of [a, b, reader]) {
          assert.equal(value.baseSHA, item.pinnedBaseSHA, 'checkout uses the explicit frozen commit');
          assert.ok(value.workspaceOwner?.attempt > 0, 'real claimed attempt owns its workspace');
          assert.equal(value.workspaceOwner.runId, item.runId);
          assert.equal(value.workspaceOwner.nodeId, value.node);
        }
        assert.notEqual(reader.workspace, a.workspace, 'reader has a fresh workspace');
        assert.notEqual(reader.workspace, b.workspace, 'reader cannot fall back to B dirty tree');
        assert.deepEqual(reader.undeclaredPeerFiles, { a: false, b: false }, 'reader receives only explicit handoff');
        assert.deepEqual(reader.aggregateFiles, { 'frontend/a.txt': null, 'backend/b.txt': null }, 'unaccepted dirty source files are not implicitly adopted');
        assert.equal(reader.declaredInputs.a.content.ownContent, a.ownContent);
        assert.equal(reader.declaredInputs.b.content.ownContent, b.ownContent);
      } else {
        assert.equal(reader.aggregateFiles['backend/b.txt'], item.artifacts.b.content.ownContent);
      }
      if (scenario === 'cancel') {
        item.measurements.bFilePreserved = item.cancel.bContentBefore === item.cancel.bContentAfter;
        assert.ok(item.measurements.bFilePreserved, 'cancel A must preserve B file');
        assert.ok(item.cancel.aStopped && item.cancel.bAliveAfterCancel && item.cancel.bHeartbeatContinued, 'real cancellation affects A while B keeps running');
        if (workspaceOnly) {
          assert.notEqual(a.workspace, item.started.a.workspace, 'retry uses a new owned workspace');
          assert.notEqual(a.branch, item.started.a.branch, 'retry uses a new private branch');
          assert.equal(a.ownVisibleBeforeWrite, false, 'retry cannot inherit cancelled dirty source');
          assert.equal(a.invocation, 2); assert.equal(b.invocation, 1);
        }
      }
      // Linked Git worktrees may share a common dir while maintaining separate
      // worktrees, indexes (gitDir) and branches. Measure that identity honestly.
      if (!item.measurements.workspaceSeparated || !item.measurements.repositorySeparated || !item.measurements.gitDirSeparated || !item.measurements.branchSeparated || a.peerVisible || b.peerVisible) {
        report.findings.push({ reuseRunSandbox: reuse, scenario, code: 'NODE_GIT_ISOLATION_MISSING', measurements: item.measurements });
      }
      persist();
    }
    proc.kill('SIGTERM'); await delay(700);
  }
  report.remoteRefsUnchanged = git(cfg.repoCloneURL, 'for-each-ref', '--format=%(refname) %(objectname)', 'refs/heads') === remotesBefore;
  assert.ok(report.remoteRefsUnchanged, 'scripts must not commit or push to the disposable remote');
  report.workspaceIsolationGatePassed = report.findings.length === 0;
  report.commitAggregationGatePassed = false;
  report.gatePassed = !workspaceOnly && report.workspaceIsolationGatePassed; persist();
  assert.equal(report.workspaceIsolationGatePassed, true, `P0-003 workspace isolation gate failed: ${JSON.stringify(report.findings)}; measurement and declared handoff completed`);
  if (workspaceOnly) t.diagnostic('Workspace/checkout/artifact slice passed; immutable result commit aggregation and cross-device transport remain unaccepted.');
});
