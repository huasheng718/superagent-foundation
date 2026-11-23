// Opt-in real exact-SHA Script/Script/Codex gates and controller-only draft PR.
// Credentials are read only from an owner-only file outside the checkout.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawn, execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, lstatSync, mkdirSync, readFileSync, realpathSync, rmSync } from 'node:fs';
import { join, relative, isAbsolute } from 'node:path';
import { api, get, post, put, list, waitFor, waitRun, track } from '../lib/api.mjs';
import { createAgent, createWorkflow, agentNode, script, trackRun } from '../lib/fixtures.mjs';
import { cfg, uid } from '../lib/env.mjs';
import { runContributionHandoff } from '../lib/contribution-handoff.mjs';

const credentialFile = process.env.AGENTFLOW_E2E_PUBLICATION_CREDENTIAL_FILE || '';
const enabled = process.env.AGENTFLOW_E2E_PUBLICATION === '1' && Boolean(credentialFile)
  && process.env.AGENTFLOW_E2E_LOCAL_MYSQL_CONTAINER === 'superagent-p0-publication'
  && ['127.0.0.1', 'localhost'].includes(new URL(cfg.base).hostname) && process.platform === 'darwin';
const entrySource = readFileSync(new URL(import.meta.url));
const digest = value => createHash('sha256').update(value).digest('hex');
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
const project = 'huasheng718/superagent-foundation';

function validationScript(kind, sha, baselineReadmeSHA256) {
  return `set -eu\nmkdir -p .agentflow/reports\npython3 - <<'PY' > .agentflow/reports/${kind}.txt\n` + String.raw`import hashlib, json, pathlib, subprocess
root = pathlib.Path.cwd()
def git(*args): return subprocess.check_output(['git', *args], text=True).strip()
assert git('rev-parse', 'HEAD') == ` + JSON.stringify(sha) + `
assert hashlib.sha256((root / 'README.md').read_bytes()).hexdigest() == '${baselineReadmeSHA256}'
files = {}
for kind in ['mac', 'linux']:
    path = root / 'contributions' / (kind + '_feature.py')
    raw = path.read_bytes(); scope = {}; exec(compile(raw, str(path), 'exec'), scope)
    assert scope['identity']() == kind
    files[str(path)] = hashlib.sha256(raw).hexdigest()
` + (kind === 'slow' ? `for repeat in range(100):
    for kind in ['mac', 'linux']:
        path = root / 'contributions' / (kind + '_feature.py')
        scope = {}; exec(compile(path.read_bytes(), str(path), 'exec'), scope)
        assert scope['identity']() == kind
` : '') + `assert git('diff', '--name-only') == ''
assert git('diff', '--cached', '--name-only') == ''
print(json.dumps({'kind': '${kind}', 'resultSHA': git('rev-parse', 'HEAD'), 'files': files, 'passed': True, 'iterations': ${kind === 'slow' ? 100 : 1}}))
PY\ncat .agentflow/reports/${kind}.txt`;
}

test('P0-005 actual exact-SHA gates publish one controller-owned draft PR', {
  skip: !enabled && 'requires explicit publication opt-in, private credentials and disposable superagent-p0-publication DB', timeout: 720_000,
}, async t => {
  const info = lstatSync(credentialFile);
  assert.ok(info.isFile() && !info.isSymbolicLink());
  assert.equal(info.mode & 0o077, 0, 'credential file must be owner-only');
  assert.equal(info.uid, process.getuid(), 'credential file must belong to the executing user');
  const rel = relative(realpathSync(join(process.cwd(), '..')), realpathSync(credentialFile));
  assert.ok(rel === '..' || rel.startsWith('../') || isAbsolute(rel), 'credential fixture must be outside the checkout');
  const credentials = JSON.parse(readFileSync(credentialFile, 'utf8'));
  assert.ok(credentials.key && credentials.baseUrl && credentials.model && credentials.gitToken, 'private credential fixture is incomplete');
  const endpoint = new URL(credentials.baseUrl);
  const hidden = [credentials.key, credentials.gitToken, credentials.baseUrl, endpoint.origin, endpoint.hostname];
  const cleanError = error => hidden.reduce((message, value) => message.replaceAll(value, '[REDACTED]'), String(error.message || error));
  try {
    await runContributionHandoff(t, {
      entrySource, entryName: '41-contribution-publication.test.mjs',
      evidenceDir: process.env.AGENTFLOW_E2E_PUBLICATION_EVIDENCE_DIR,
      repositoryMetadata: { provider: 'github', gongfengProjectId: `github:${project}`, defaultBranch: 'main' },
      sensitiveValues: [credentials.key, credentials.gitToken], sensitiveEndpoints: [credentials.baseUrl, endpoint.origin, endpoint.hostname],
      onIntegrated: async context => {
        const { report, dir, nativeBinary, repositoryId, persist, addSecret, onBeforeCleanup, onAfterCleanup, restoreReadOnlySource } = context;
        report.gatePassed = false;
        const proof = report.publicationAcceptance = { state: 'incomplete', runs: [], denials: {}, model: credentials.model,
          scope: 'actual API gates and real draft PR; no human business acceptance or merge', controllerCredentialOnly: true };
        let runnerLog = '', proc, session, runnerId;
        const privatePaths = ['gate-home', 'gate-tmp', 'gate-workspace', 'gate-state.json'];
        const snapshot = async () => {
          for (const item of proof.runs) {
            try {
              item.run = await get(`/api/runs/${item.runId}`);
              item.tasks = list(await get('/api/runners/tasks')).filter(task => task.runId === item.runId);
              item.events = [];
              for (const task of item.tasks) if (session?.sessionToken) item.events.push({ taskId: task.id,
                response: await get(`/api/runners/tasks/${task.id}/events`, { auth: false, headers: { Authorization: `Bearer ${session.sessionToken}` } }) });
            } catch (error) { item.snapshotError = cleanError(error); }
          }
          proof.runnerLog = runnerLog; persist();
        };
        onBeforeCleanup(snapshot);
        onAfterCleanup(async () => {
          proc?.kill('SIGTERM');
          if (proc && proc.exitCode === null && proc.signalCode === null) await Promise.race([new Promise(resolve => proc.once('exit', resolve)), pause(5000)]);
          if (proc && proc.exitCode === null && proc.signalCode === null) proc.kill('SIGKILL');
          if (session?.sessionToken) await api('POST', '/runners/deregister', {}, { auth: false, headers: { Authorization: `Bearer ${session.sessionToken}` }, expect: [200, 401] });
          for (const path of privatePaths) rmSync(join(dir, path), { recursive: true, force: true });
          proof.cleanup = { processStopped: !proc || proc.exitCode !== null || proc.signalCode !== null, privatePathsRemoved: privatePaths.every(path => !existsSync(join(dir, path))) };
          proof.runnerLog = runnerLog;
        });
        const github = async path => {
          const res = await fetch(`https://api.github.com/repos/${project}${path}`, { headers: { Accept: 'application/vnd.github+json', Authorization: `Bearer ${credentials.gitToken}`, 'X-GitHub-Api-Version': '2022-11-28' }, signal: AbortSignal.timeout(45_000) });
          assert.equal(res.status, 200, `independent GitHub read ${path}: HTTP ${res.status}`);
          return res.json();
        };
        const beforeMain = await github('/git/ref/heads/main');
        assert.equal(beforeMain.object.sha, report.baseSHA, 'public main must match the fixed source');
        proof.mainBefore = beforeMain.object.sha; persist();
        await restoreReadOnlySource();
        const owner = list(await get('/api/users')).find(user => user.loginName === 'e2e-local');
        assert.ok(owner);
        await put(`/api/users/${owner.id}/credentials`, { codexApiKey: credentials.key, codexBaseUrl: credentials.baseUrl, gitToken: credentials.gitToken });
		// Installed clean-HOME Runners require the actual internal gate sidecar;
		// do not rely on source-root discovery through an inherited host cwd.
		const gateBinary = join(dir, 'agentflow-gate-mcp');
		execFileSync('go', ['build', '-o', gateBinary, './cmd/agentflow-gate-mcp'], {
		  cwd: realpathSync(join(process.cwd(), '..')), env: { ...process.env, CGO_ENABLED: '0', GOOS: process.platform, GOARCH: process.arch === 'x64' ? 'amd64' : process.arch }, stdio: 'pipe',
		});
		proof.gateBinarySHA256 = digest(readFileSync(gateBinary)); persist();
        for (const path of ['gate-home', 'gate-tmp']) mkdirSync(join(dir, path), { mode: 0o700 });
        const { code } = await post('/api/runners/bind-codes', {}); addSecret(code);
        const name = uid('publication-gates');
        proc = spawn(nativeBinary, ['--server', cfg.base, '--name', name, '--state', join(dir, 'gate-state.json'), '--workspace', join(dir, 'gate-workspace'), '--heartbeat', '2s', '--auto-update=false'], {
          cwd: dir, env: { PATH: process.env.PATH, HOME: join(dir, 'gate-home'), TMPDIR: join(dir, 'gate-tmp'), LANG: 'en_US.UTF-8', AGENTFLOW_BROWSER: 'off', AGENTFLOW_RUNNER_BOOTSTRAP_TOKEN: code }, stdio: ['ignore', 'pipe', 'pipe'],
        });
        for (const stream of [proc.stdout, proc.stderr]) stream.on('data', data => { runnerLog += data; });
        proc.on('error', error => { runnerLog += '\nspawn: ' + cleanError(error); });
        const runner = await waitFor('isolated gate Runner Codex runtime online', async () => list(await get('/api/runners')).find(r => r.name === name && r.status === 'online' && r.runtimes?.some(rt => rt.provider === 'codex' && rt.status === 'online')), { timeoutMs: 90_000, intervalMs: 500 });
        runnerId = runner.id;
        session = JSON.parse(readFileSync(join(dir, 'gate-state.json'), 'utf8')); addSecret(session.sessionToken);
        await put('/api/runners/runtimes', { runnerId, provider: 'codex', authMode: 'personal' });
        const configured = list(await get('/api/runners')).find(r => r.id === runnerId);
        assert.equal(configured.runtimes.find(rt => rt.provider === 'codex').authMode, 'personal');
        proof.runner = { id: runnerId, os: runner.os, arch: runner.arch, privateHOME: true, inheritedHostingCredential: false, runtime: configured.runtimes.find(rt => rt.provider === 'codex') };
        await put('/api/runners/default', { runnerId });
        const resultSHA = report.integration.resultSHA;
        const dispatch = async (label, nodes, sha = resultSHA) => {
          const workflowId = await createWorkflow(`publication-${label}`, nodes, { sandboxProfileId: 'local' });
          const issue = await post('/api/issues', { projectId: cfg.projectId, title: `E2E publication ${label} ${cfg.runTag}`, description: 'exact-SHA gate; no merge or business completion', priority: 'medium', kind: 'requirement', status: 'todo', workflowId, repositoryIds: [repositoryId] });
          track(`issue ${issue.id}`, () => post(`/api/issues/${issue.id}/close`, {}, { expect: [200, 404, 409] }));
          await post(`/api/issues/${issue.id}/dispatch`, { workflowId, runnerWorkspaceLayout: 'attempt', workspaceBaseSHAs: { [repositoryId]: sha } });
          const summary = await waitFor('gate Run frozen', async () => list(await get(`/api/issues/${issue.id}/runs`))[0], { timeoutMs: 60_000, intervalMs: 300 });
          const runId = summary.id || summary.runId; trackRun(runId);
          const item = { label, runId, expectedSHA: sha }; proof.runs.push(item); persist();
          const run = await waitRun(runId, r => r.status === 'succeeded', { timeoutMs: 360_000, label: `${label} succeeded` });
          assert.equal(run.executionContext.executionRunnerId, runnerId);
          assert.equal(run.executionContext.runnerWorkspaceLayout, 'attempt');
          assert.equal(run.executionContext.workspaceBaseSHAs[repositoryId], sha);
          item.run = run; item.tasks = list(await get('/api/runners/tasks')).filter(task => task.runId === runId && !task.action);
          for (const task of item.tasks) {
            assert.equal(task.status, 'success'); assert.equal(task.acceptedContributions?.length, 1);
            assert.equal(task.acceptedContributions[0].baseSHA, sha); assert.equal(task.acceptedContributions[0].resultSHA, sha);
            assert.equal(task.deliveryPolicy, 'controlled_pr');
            for (const repo of task.invocation?.repositories || []) assert.ok(!repo.gitToken);
            assert.ok(!task.invocation?.repository?.gitToken);
          }
          persist(); return item;
        };
        const endpointPath = `/api/runs/${report.runs[0].runId}/contributions/${report.integration.id}`;
        proof.denials.beforeGates = (await post(`${endpointPath}/publish`, {}, { expect: 409, raw: true })).status;
        for (const operation of ['gates', 'publish']) {
          proof.denials[`nonAdmin-${operation}`] = (await post(`${endpointPath}/${operation}`, {}, { token: cfg.tokenB, expect: 403, raw: true })).status;
          proof.denials[`runner-${operation}`] = (await post(`${endpointPath}/${operation}`, {}, { auth: false, headers: { Authorization: `Bearer ${session.sessionToken}` }, expect: [401, 403], raw: true })).status;
        }
        const wrong = await dispatch('wrong-pin', [script('wrong', 'printf "Wrong SHA gate evidence\\n"')], report.baseSHA);
        const wrongArtifact = wrong.run.artifacts.find(a => a.nodeId === 'wrong' && a.name === 'result'); assert.ok(wrongArtifact);
        proof.denials.wrongSHA = (await post(`${endpointPath}/gates`, { gateRunId: wrong.runId, nodeId: 'wrong', kind: 'fast', artifactId: wrongArtifact.id }, { expect: 409, raw: true })).status;
        const outputs = kind => [{ name: `${kind}-report`, path: `.agentflow/reports/${kind}.txt`, mediaType: 'text/plain', required: true }];
        const reviewer = await createAgent('publication-independent-review', `Independently review the only repository checkout in this workspace at exact commit ${resultSHA}. Inspect contributions/mac_feature.py and contributions/linux_feature.py, verify identity() returns mac and linux respectively by executing the sources using python3 without importing or writing bytecode, inspect the diff from ${report.baseSHA}, and check README.md is unchanged. These are the only intended product changes. Do not modify any tracked file, install packages, access network, run git commit/push or access files outside the workspace. Execute real checks and report concrete evidence. Call submit_gate_result with pass only if the checks pass and no high/critical defect is found; otherwise submit fail/human as appropriate. Do not fabricate a verdict.`, { executorId: 'codex', model: credentials.model, loop: { enabled: false } });
        const gates = await dispatch('exact-sha', [
          script('fast', validationScript('fast', resultSHA, report.baselineReadmeSHA256), { executionScope: 'repositories', artifactOutputs: outputs('fast') }),
          script('slow', validationScript('slow', resultSHA, report.baselineReadmeSHA256), { executionScope: 'repositories', artifactOutputs: outputs('slow') }),
          agentNode('review', reviewer, { executorId: 'codex', maxDurationSec: 240 }),
        ]);
        assert.ok(gates.run.nodes.review.sessionId);
        assert.equal(gates.run.nodes.review.gateResult?.decision, 'pass');
        assert.ok(gates.tasks.some(task => task.provider === 'codex' && task.invocation.credentialSource === 'personal'));
        assert.equal(new Set(gates.tasks.map(task => task.id)).size, 3, 'three separate execution tasks');
        proof.denials.wrongArtifact = (await post(`${endpointPath}/gates`, { gateRunId: gates.runId, nodeId: 'fast', kind: 'fast', artifactId: wrongArtifact.id }, { expect: 409, raw: true })).status;
        let publication;
        for (const kind of ['fast', 'slow', 'review']) {
          const artifact = gates.run.artifacts.find(a => a.nodeId === kind && a.attempt === gates.run.nodes[kind].attempt && a.name === (kind === 'review' ? 'result' : `${kind}-report`));
          assert.ok(artifact && artifact.size > 0);
          const content = await get(`/api/runs/${gates.runId}/artifacts/${encodeURIComponent(artifact.id)}`, { raw: true });
          assert.equal(digest(content.text), artifact.sha256);
          publication = await post(`${endpointPath}/gates`, { gateRunId: gates.runId, nodeId: kind, kind, artifactId: artifact.id });
          proof.latestPublication = publication; persist();
          assert.equal(publication.state, kind === 'review' ? 'ready' : 'gating');
        }
        assert.equal(new Set(publication.gates.map(g => g.gateTaskId)).size, 3);
        for (const gate of publication.gates) { assert.equal(gate.resultSHA, resultSHA); assert.equal(gate.manifestSHA256, report.integration.manifestSHA256); }
        publication = await post(`${endpointPath}/publish`, {}, { timeoutMs: 120_000 });
        proof.latestPublication = publication; persist(); // Keep remote receipt even if a later independent check fails.
        assert.equal(publication.state, 'pr_ready'); assert.equal(publication.resultSHA, resultSHA);
        assert.ok(publication.prNumber > 0); assert.equal(publication.prURL, `https://github.com/${project}/pull/${publication.prNumber}`);
        const replay = await post(`${endpointPath}/publish`, {}, { timeoutMs: 120_000 });
        assert.equal(replay.prURL, publication.prURL); assert.equal(replay.branch, publication.branch); assert.equal(replay.attempt, publication.attempt);
        const stored = await get(`${endpointPath}/publication`); assert.deepEqual(stored, replay);
        const pull = await github(`/pulls/${publication.prNumber}`);
        assert.equal(pull.head.sha, resultSHA); assert.equal(pull.head.ref, publication.branch);
        assert.equal(pull.head.repo.full_name, project); assert.equal(pull.base.repo.full_name, project);
        assert.equal(pull.base.ref, 'main'); assert.equal(pull.state, 'open'); assert.equal(pull.draft, true);
        assert.equal(pull.merged, false); assert.equal(pull.auto_merge, null);
        const files = await github(`/pulls/${publication.prNumber}/files?per_page=100`);
        assert.deepEqual(files.map(file => file.filename).sort(), ['contributions/linux_feature.py', 'contributions/mac_feature.py']);
        const pulls = await github(`/pulls?state=all&head=${encodeURIComponent('huasheng718:' + publication.branch)}&per_page=100`);
        assert.equal(pulls.length, 1, 'replay must not create another PR');
        proof.mainAfter = (await github('/git/ref/heads/main')).object.sha; assert.equal(proof.mainAfter, proof.mainBefore);
        proof.remote = { url: pull.html_url, number: pull.number, headSHA: pull.head.sha, branch: pull.head.ref, target: pull.base.ref, draft: pull.draft, state: pull.state, merged: pull.merged, autoMerge: pull.auto_merge, changedFiles: files.map(file => file.filename), matchingPRs: pulls.length };
        proof.state = 'verified'; report.gatePassed = true; report.boundaries.modelUsed = true;
        report.boundaries.publication = 'one real draft GitHub PR, replay verified; no merge';
        report.acceptanceLevel = 'local_mac_linux_immutable_handoff_actual_exact_sha_gates_and_draft_pr';
        await snapshot();
        t.diagnostic(`controller draft PR: ${publication.prURL}`);
      },
    });
  } catch (error) { throw new Error(cleanError(error)); }
});
