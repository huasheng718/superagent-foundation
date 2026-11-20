// Opt-in actual Codex extraction and reviewed-memory comparison, local only.
// This file starts nothing unless all explicit disposable-pilot flags are set.
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { spawn, execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { createServer } from 'node:http';
import { createReadStream, chmodSync, existsSync, lstatSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { cfg, uid } from '../lib/env.mjs';
import { api, get, post, put, del, list, waitFor, ensureProject, cleanup, track } from '../lib/api.mjs';
import { agentNode, createAgent, createWorkflow, trackRun } from '../lib/fixtures.mjs';

const credentialFile = process.env.AGENTFLOW_E2E_CODEX_CREDENTIAL_FILE || '';
const enabled = process.env.AGENTFLOW_E2E_KNOWLEDGE_EXTRACTION === '1'
  && process.env.AGENTFLOW_E2E_KNOWLEDGE_OWNER_REVIEW === '1'
  && process.env.AGENTFLOW_E2E_LOCAL_MYSQL_CONTAINER === 'superagent-p0-contribution'
  && process.env.AGENTFLOW_E2E_MEMORY_PROVIDER === 'tencentdb-private'
  && cfg.projectId === 'e2e-auto-cap003' && Boolean(credentialFile)
  && ['127.0.0.1', 'localhost'].includes(new URL(cfg.base).hostname) && process.platform === 'darwin';
const artifactPath = '.agentflow/artifacts/knowledge-candidates.json';
const evaluationPath = '.agentflow/artifacts/policy-evaluation.json';
const skillId = 'cap003-review-settlement-policy';
const budget = { mode: 'metered', maxTotalTokens: 131072, maxInvocations: 1 };
const hash = value => createHash('sha256').update(value).digest('hex');
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const git = (dir, ...args) => execFileSync('git', ['-C', dir, '-c', 'core.hooksPath=/dev/null', ...args], {
  encoding: 'utf8', env: { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null', GIT_TERMINAL_PROMPT: '0',
    GIT_AUTHOR_DATE: '2026-11-18T00:00:00Z', GIT_COMMITTER_DATE: '2026-11-18T00:00:00Z' },
}).trim();
const fixture = {
  'README.md': '# Cobalt synthetic settlement fixture\n\nAll facts are synthetic. Read docs/settlement-policy.md. This repository contains no customer or upstream source.\n',
  'docs/settlement-policy.md': '# Cobalt settlement policy\n\nThe deduplicationKey is requestId.\nThe maxChargeAttempts is 3 (the first attempt counts).\nThe onDuplicate action is return_existing_receipt. Never charge a duplicate again.\nThe onAttemptExhausted action is human_review. No further automatic charge is permitted.\nWhen both conditions hold, retain both actions and set mayChargeAgain to false.\n',
  'NOTICE.md': 'Synthetic test material created for this local acceptance experiment. No upstream materials or real business policy are included.\n',
};
const expectedEvaluation = { deduplicationKey: 'requestId', maxChargeAttempts: 3, onDuplicate: 'return_existing_receipt', onAttemptExhausted: 'human_review', mayChargeAgain: false };

test('CAP003 actual extraction → two reviewed candidates → BM25 receipt → fresh Runner with no-memory comparison', {
  skip: !enabled && 'requires explicit disposable local Codex/private-provider pilot and synthetic owner-review flags', timeout: 900_000,
}, async t => {
  assert.equal(cfg.keep, false, 'pilot cleanup cannot be disabled');
  assert.equal(statSync(credentialFile).mode & 0o077, 0, 'credentials must be owner-only');
  const credentials = JSON.parse(readFileSync(credentialFile, 'utf8'));
  assert.ok(credentials.key && credentials.baseUrl && credentials.model);
  const root = git(process.cwd(), 'rev-parse', '--show-toplevel');
  const dir = mkdtempSync(join(tmpdir(), 'cap003-owned-runtime-'));
  chmodSync(dir, 0o700);
  const evidence = resolve(process.env.AGENTFLOW_E2E_KNOWLEDGE_EVIDENCE_DIR || mkdtempSync(join(tmpdir(), 'cap003-evidence-')));
  assert.ok(evidence !== dir && !evidence.startsWith(dir + '/'), 'evidence must survive runtime removal');
  mkdirSync(evidence, { recursive: true, mode: 0o700 });
  const secrets = new Set([cfg.token, cfg.tokenB, credentials.key, credentials.baseUrl].filter(Boolean));
  const processes = [], runs = [], releases = [];
  let sourceServer, memory, candidate, activeSkill, previousDefault, repositoryId = '';
  const report = { startedAt: new Date().toISOString(), acceptance: 'incomplete', productHEAD: git(root, 'rev-parse', 'HEAD'),
    harnessSHA256: hash(readFileSync(new URL(import.meta.url))), configuredModel: credentials.model, runs: [], runners: [], sourceRequests: [],
    boundaries: { actualModelRequired: true, modelInvocationLimit: 3, oneInvocationPerRun: true, nodeDeadlineSec: 180,
      budget, financialHardCap: false, cost: 'observed status retained; unknown is not zero', privateProvider: 'prestarted and owned by pilot operator',
      source: 'fixed synthetic commit, read-only HTTP, no product edits', physicalDevices: 1, hostedWrites: false,
      review: 'explicit opt-in authenticated API-owner review of synthetic output; not human business acceptance',
      comparison: 'one sequential case; same source, task, model, release and skill; only approved memory/receipt differ; no causal or general quality claim' },
    cleanupErrors: [],
  };
  const redact = value => {
    let raw = JSON.stringify(value, (key, item) => /apiKey|authToken|sessionToken|gitToken|password|secret|authorization/i.test(key) ? '[REDACTED]' : item, 2);
    for (const secret of secrets) raw = raw.replaceAll(secret, '[REDACTED]');
    return raw;
  };
  const persist = () => {
    writeFileSync(join(evidence, 'cap003-facts.json'), redact(report) + '\n', { mode: 0o600 });
    writeFileSync(join(evidence, '46-knowledge-extraction.test.mjs'), readFileSync(new URL(import.meta.url)), { mode: 0o600 });
    for (const item of processes) {
      let log = item.log;
      for (const secret of secrets) log = log.replaceAll(secret, '[REDACTED]');
      writeFileSync(join(evidence, `${item.kind}-runner.log`), log, { mode: 0o600 });
    }
  };
  const attempt = async (label, fn) => { try { return await fn(); } catch (error) { report.cleanupErrors.push(`${label}: ${error.message}`); } };
  const telemetry = async id => {
    const events = []; let after = 0;
    for (let page = 0; page < 40; page++) {
      const result = await get(`/api/runs/${id}/telemetry?expanded=true&after=${after}&limit=200`);
      events.push(...result.events);
      if (!result.hasMore) return { events, complete: true };
      assert.ok(result.nextAfter > after, 'telemetry pagination must advance'); after = result.nextAfter;
    }
    return { events, complete: false };
  };
  const stop = async item => {
    item.proc.kill('SIGTERM');
    if (item.proc.exitCode === null && item.proc.signalCode === null) await Promise.race([new Promise(resolveExit => item.proc.once('exit', resolveExit)), delay(5_000)]);
    if (item.proc.exitCode === null && item.proc.signalCode === null) {
      item.proc.kill('SIGKILL');
      await Promise.race([new Promise(resolveExit => item.proc.once('exit', resolveExit)), delay(2_000)]);
    }
  };
  t.after(async () => {
    for (const id of runs) await attempt(`snapshot ${id}`, async () => {
      const run = await get(`/api/runs/${id}`);
      const tasks = list(await get('/api/runners/tasks')).filter(task => task.runId === id);
      const snapshot = { run, tasks, telemetry: await telemetry(id), runnerEvents: [] }; report.runs.push(snapshot);
      for (const task of tasks) {
        const ownedRunner = processes.find(item => item.runner?.id === (task.runnerId || task.leaseOwner));
        if (ownedRunner?.sessionToken) snapshot.runnerEvents.push({ taskId: task.id,
          response: await get(`/api/runners/tasks/${task.id}/events`, { auth: false, headers: { Authorization: `Bearer ${ownedRunner.sessionToken}` } }) });
      }
    });
    // Preserve only the two declared synthetic JSON report names, including
    // failure before upload; never copy Codex config/HOME or arbitrary files.
    report.ownedReportSnapshots = [];
    let visited = 0;
    const preserveReports = (path, depth = 0) => {
      if (!existsSync(path) || depth > 16 || visited++ > 20_000) return;
      for (const entry of readdirSync(path, { withFileTypes: true })) {
        if (entry.isDirectory() && !['.git', 'config', 'skills', 'node_modules'].includes(entry.name)) preserveReports(join(path, entry.name), depth + 1);
        else if (entry.isFile() && ['knowledge-candidates.json', 'policy-evaluation.json'].includes(entry.name)) {
          const file = join(path, entry.name), stat = lstatSync(file);
          if (![artifactPath, evaluationPath].some(declared => file.endsWith('/' + declared))) continue;
          if (!stat.isFile() || stat.size > 256 * 1024) continue;
          const bytes = readFileSync(file), sha256 = hash(bytes), saved = `owned-report-${report.ownedReportSnapshots.length}-${entry.name}`;
          let text = bytes.toString('utf8'); for (const secret of secrets) text = text.replaceAll(secret, '[REDACTED]');
          writeFileSync(join(evidence, saved), text, { mode: 0o600 });
          report.ownedReportSnapshots.push({ name: entry.name, saved, size: stat.size, rawSHA256: sha256, savedSHA256: hash(text), sourceRunSuccessRequiredBeforeIngest: true });
        }
      }
    };
    for (const item of processes) await attempt(`preserve owned reports ${item.kind}`, () => preserveReports(join(item.owned, 'workspace')));
    if (memory?.state === 'approved') await attempt('revoke native memory', async () => {
      memory = await post(`/api/projects/${cfg.projectId}/memories/${memory.id}/decision`, { decision: 'revoked', expectedRevision: memory.revision, reason: 'Owned synthetic CAP003 fixture cleanup' });
      report.nativeMemoryAfterCleanup = memory;
      report.providerCleanup = await post(`/api/projects/${cfg.projectId}/memory-provider/sync`, { limit: 10 });
      const recalled = await post(`/api/projects/${cfg.projectId}/memory-recall`, { query: 'requestId', limit: 10, includeReceipt: true });
      assert.ok(!list(recalled.items).some(item => item.id === memory.id));
      report.revokedRecall = recalled;
    });
    for (const release of releases) await attempt(`revoke release ${release.id}`, async () => {
      const current = await get(`/api/projects/${cfg.projectId}/team-releases/${release.id}`);
      if (current.state === 'published') await post(`/api/projects/${cfg.projectId}/team-releases/${release.id}/revoke`, { expectedRevision: current.revision, reason: 'Owned CAP003 fixture cleanup' });
    });
    persist();
    await cleanup();
    // Catalog deletion requires the evaluator Agent's skill reference to be
    // gone, so remove the promoted resource after tracked Agent cleanup.
    if (activeSkill) await attempt('delete promoted skill', async () => {
      const current = list(await get('/api/skills')).find(skill => skill.id === activeSkill.id);
      if (current) await del(`/api/skills?id=${encodeURIComponent(current.id)}`, { headers: { 'If-Match': JSON.stringify(current.sourceRevision) } });
    });
    if (previousDefault !== undefined) await attempt('restore previous default', () => put('/api/runners/default', { runnerId: previousDefault }, { expect: [200, 404] }));
    for (const item of processes) {
      await attempt(`stop ${item.kind}`, () => stop(item));
      if (item.sessionToken) await attempt(`deregister ${item.kind}`, () => api('POST', '/runners/deregister', {}, { auth: false, headers: { Authorization: `Bearer ${item.sessionToken}` }, expect: [200, 401] }));
    }
    sourceServer?.closeAllConnections();
    if (sourceServer?.listening) await new Promise(resolveClose => sourceServer.close(resolveClose));
    rmSync(dir, { recursive: true, force: true });
    const deletedRuns = [];
    for (const id of runs) await attempt(`verify Run deletion ${id}`, async () => {
      const response = await api('GET', `/api/runs/${id}`, undefined, { raw: true, expect: [200, 404] });
      assert.equal(response.status, 404); deletedRuns.push(id);
    });
    report.finishedAt = new Date().toISOString();
    report.cleanup = { stoppedRunners: processes.map(item => ({ kind: item.kind, stopped: item.proc.exitCode !== null || item.proc.signalCode !== null })),
      sourceServerStopped: !sourceServer?.listening, privateRuntimeRemoved: !existsSync(dir), deletedRuns,
      retainedAudit: { extractionReceiptId: report.import?.receipt?.id || null, memoryId: memory?.id || null, skillCandidateId: candidate?.id || null,
        releaseIds: releases.map(item => item.id), fixedProjectId: cfg.projectId, providerLedgerCreated: Boolean(report.provider) },
      platformCredentialErasure: 'operator must remove this disposable platform schema/config after evidence collection',
      providerPhysicalErasure: 'operator must stop/remove its owned generation, including JSONL; this test only revokes native data and performs exact scope cleanup' };
    persist();
    t.diagnostic(`CAP003 evidence: ${evidence}`);
    assert.equal(report.cleanupErrors.length, 0, report.cleanupErrors.join('\n'));
    assert.ok(report.cleanup.stoppedRunners.every(item => item.stopped));
    assert.equal(report.cleanup.sourceServerStopped, true);
    assert.equal(report.cleanup.privateRuntimeRemoved, true);
  });

  await ensureProject();
  const base = `/api/projects/${cfg.projectId}`;
  const sandboxId = uid('cap003-local');
  track(`sandbox ${sandboxId}`, () => del(`/api/sandboxes?id=${encodeURIComponent(sandboxId)}`));
  await put('/api/sandboxes', { id: sandboxId, name: 'CAP003 owned local sandbox', projectId: cfg.projectId, driver: 'local', readOnlyRootFS: false });
  report.sandboxId = sandboxId;
  assert.ok(!list(await get(base + '/memories')).some(item => item.state === 'approved'), 'use a dedicated project without other approved memories');
  assert.ok(!list(await get('/api/skills')).some(skill => skill.id === skillId), 'pilot skill ID already exists; do not overwrite');
  previousDefault = (await get('/api/runners/default')).runnerId || '';
  const user = list(await get('/api/users')).find(item => item.loginName === 'e2e-local');
  assert.ok(user, 'dedicated local acceptance identity required');
  await put(`/api/users/${user.id}/credentials`, { codexApiKey: credentials.key, codexBaseUrl: credentials.baseUrl });
  const seed = join(dir, 'seed'), source = join(dir, 'source.git');
  mkdirSync(seed); git(seed, 'init', '-q', '-b', 'main');
  for (const [path, content] of Object.entries(fixture)) {
    mkdirSync(join(seed, path, '..'), { recursive: true }); writeFileSync(join(seed, path), content);
  }
  git(seed, 'add', '.');
  git(seed, '-c', 'user.name=CAP003 synthetic fixture', '-c', 'user.email=cap003@example.invalid', 'commit', '-q', '-m', 'Fixed synthetic settlement policy');
  const commit = git(seed, 'rev-parse', 'HEAD');
  git(dir, 'clone', '--bare', '--quiet', seed, source); git(source, 'update-server-info');
  const refs = git(source, 'for-each-ref', '--format=%(refname) %(objectname)');
  assert.equal(refs, `refs/heads/main ${commit}`);
  report.syntheticSource = { commit, refs, files: Object.fromEntries(Object.entries(fixture).map(([path, content]) => [path, { content, sha256: hash(content) }])) };
  sourceServer = createServer((req, res) => {
    let path;
    try { path = decodeURIComponent(new URL(req.url, 'http://fixture.invalid').pathname); } catch { res.writeHead(400).end(); return; }
    const entry = { method: req.method, path, credentialPresented: Boolean(req.headers.authorization), status: 404 };
    report.sourceRequests.push(entry);
    if (!['GET', 'HEAD'].includes(req.method)) { entry.status = 405; res.writeHead(405).end(); return; }
    if (entry.credentialPresented || !/^\/source\.git\/(?:HEAD|info\/refs|objects\/(?:info\/packs|[0-9a-f]{2}\/[0-9a-f]{38}|pack\/pack-[0-9a-f]{40}\.(?:idx|pack)))$/.test(path)) { res.writeHead(404).end(); return; }
    const file = join(source, path.slice('/source.git/'.length));
    if (!existsSync(file) || !statSync(file).isFile()) { res.writeHead(404).end(); return; }
    entry.status = 200; res.writeHead(200, { 'Content-Type': 'application/octet-stream', 'Content-Length': statSync(file).size, 'Cache-Control': 'no-store' });
    if (req.method === 'HEAD') res.end(); else createReadStream(file).pipe(res);
  });
  await new Promise(resolveListen => sourceServer.listen(0, '127.0.0.1', resolveListen));
  const sourceURL = `http://127.0.0.1:${sourceServer.address().port}/source.git`;
  secrets.add(sourceURL);
  repositoryId = uid('cap003-source');
  track(`repository ${repositoryId}`, () => del(`/api/repositories?id=${repositoryId}`));
  await post('/api/repositories', { id: repositoryId, projectId: cfg.projectId, provider: 'gongfeng', name: repositoryId,
    gongfengProjectId: `e2e-auto/${repositoryId}`, cloneUrl: sourceURL, defaultBranch: 'main' });
  report.repositoryId = repositoryId;
  const nativeBinary = join(dir, 'agentflow-runner'), gateBinary = join(dir, 'agentflow-gate-mcp');
  const binary = await fetch(`${cfg.base}/runners/agentflow-runner?os=darwin&arch=${process.arch === 'arm64' ? 'arm64' : 'amd64'}`, { signal: AbortSignal.timeout(120_000) });
  assert.equal(binary.status, 200); writeFileSync(nativeBinary, Buffer.from(await binary.arrayBuffer())); chmodSync(nativeBinary, 0o700);
  execFileSync('go', ['build', '-o', gateBinary, './cmd/agentflow-gate-mcp'], { cwd: root, stdio: 'pipe' });
  report.binaries = { runnerSHA256: hash(readFileSync(nativeBinary)), gateSHA256: hash(readFileSync(gateBinary)) };
  const launchRunner = async kind => {
    const owned = join(dir, kind); mkdirSync(owned); mkdirSync(join(owned, 'home')); mkdirSync(join(owned, 'tmp'));
    const { code } = await post('/api/runners/bind-codes', {}); secrets.add(code);
    const name = uid(`cap003-${kind}`);
    const proc = spawn(nativeBinary, ['--server', cfg.base, '--name', name, '--state', join(owned, 'state.json'), '--workspace', join(owned, 'workspace'), '--heartbeat', '2s', '--auto-update=false'], {
      cwd: owned, env: { PATH: process.env.PATH, HOME: join(owned, 'home'), TMPDIR: join(owned, 'tmp'), LANG: 'en_US.UTF-8', AGENTFLOW_BROWSER: 'off', AGENTFLOW_GATE_MCP_BINARY: gateBinary, AGENTFLOW_RUNNER_BOOTSTRAP_TOKEN: code }, stdio: ['ignore', 'pipe', 'pipe'],
    });
    const item = { kind, proc, log: '', owned }; processes.push(item);
    for (const stream of [proc.stdout, proc.stderr]) stream.on('data', bytes => { item.log += bytes; });
    proc.on('error', error => { item.log += '\n' + error.message; });
    const runner = await waitFor(`${kind} Runner online`, async () => list(await get('/api/runners')).find(value => value.name === name && value.status === 'online'), { timeoutMs: 45_000, intervalMs: 500 });
    const state = JSON.parse(readFileSync(join(owned, 'state.json'), 'utf8'));
    assert.ok(state.sessionToken); item.sessionToken = state.sessionToken; secrets.add(state.sessionToken); item.runner = runner;
    const version = runner.runnerVersion.replace(/^v/, '').split('.').map(Number);
    assert.ok(version[0] > 0 || version[1] >= 13, 'fresh Runner must support recall receipt metadata (>=0.13.0)');
    await put('/api/runners/runtimes', { runnerId: runner.id, provider: 'codex', authMode: 'personal' });
    await put('/api/runners/default', { runnerId: runner.id });
    report.runners.push({ kind, id: runner.id, version: runner.runnerVersion, os: runner.os, arch: runner.arch, freshHome: true }); persist();
    return item;
  };
  const publish = async (name, agentId) => {
    const previous = list(await get(base + '/team-releases'));
    const release = await post(base + '/team-releases', { name, agentIds: [agentId], expectedBaseVersion: Math.max(0, ...previous.map(item => item.version)) });
    releases.push(release); return release;
  };
  const execute = async ({ name, runner, agentId, release, path, selection }) => {
    const workflowId = await createWorkflow(name, [agentNode('work', agentId, { executorId: 'codex', maxAttempts: 1, maxDurationSec: 180,
      artifactOutputs: [{ name: name === 'cap003-extract' ? 'knowledge-candidates' : 'policy-evaluation', path, required: true, mediaType: 'application/json' }] })], { sandboxProfileId: sandboxId });
    const started = Date.now();
    const run = await post('/api/runs', { workflowId, projectId: cfg.projectId, input: name === 'cap003-extract' ? 'Extract reviewed-policy suggestions from the synthetic fixed source.' : 'Evaluate duplicate=true and attempt=4 for the fixed Cobalt settlement policy.',
      executionBudget: budget, repositoryIds: [repositoryId], runnerWorkspaceLayout: 'attempt', workspaceBaseSHAs: { [repositoryId]: commit },
      nodePlacements: { work: { runnerId: runner.id, os: runner.os, arch: runner.arch } }, teamContext: { releaseId: release.id, ...selection } });
    runs.push(run.id); trackRun(run.id); persist();
    const done = await waitFor(name, async () => {
      const current = await get(`/api/runs/${run.id}`);
      if (['failed', 'cancelled', 'rejected'].includes(current.status)) throw new Error(`${name} failed: ${JSON.stringify(current.nodes)}`);
      return current.status === 'succeeded' ? current : null;
    }, { timeoutMs: 240_000, intervalMs: 500 });
    const elapsedMS = Date.now() - started;
    const tasks = list(await get('/api/runners/tasks')).filter(task => task.runId === run.id && task.nodeId === 'work' && !task.action);
    assert.equal(tasks.length, 1, 'one actual model invocation without automatic retry');
    const task = tasks[0], node = done.nodes.work;
    assert.equal(task.id, node.executionTaskId); assert.equal(task.provider, 'codex'); assert.equal(task.status, 'success'); assert.ok(node.sessionId);
    assert.equal(task.invocation.credentialSource, 'personal'); assert.equal(task.attempt, 1); assert.equal(node.attempt, 1);
    assert.equal(done.executionBudget.invocations, 1); assert.ok(!done.executionBudget.stopReason); assert.equal(task.invocation.resumeSessionId || '', '');
    assert.equal(task.acceptedContributions.length, 1);
    const contribution = task.acceptedContributions[0];
    assert.equal(contribution.baseSHA, commit); assert.equal(contribution.resultSHA, commit, 'excluded report must leave source tree unchanged');
    assert.equal(contribution.contextSHA256, task.outputs.contextEvidence.contextSHA256);
    assert.equal(task.outputs.contextEvidence.intelligenceSHA256, done.intelligence.sha256);
    const artifact = done.artifacts.find(item => item.nodeId === 'work' && item.path === path); assert.ok(artifact);
    const raw = await api('GET', `/api/runs/${run.id}/artifacts/${encodeURIComponent(artifact.id)}`, undefined, { raw: true });
    assert.ok(Buffer.byteLength(raw.text) <= 256 * 1024); assert.equal(hash(raw.text), artifact.sha256); assert.equal(Buffer.byteLength(raw.text), artifact.size);
    assert.ok(![...secrets].some(secret => raw.text.includes(secret)), 'declared synthetic report must not contain private credential or endpoint values');
    writeFileSync(join(evidence, `${name}.json`), raw.text, { mode: 0o600 });
    const usage = node.usage || {}, actualTelemetry = await telemetry(run.id);
    const observed = actualTelemetry.events.filter(event => event.nodeId === 'work' && event.model).map(event => ({ model: event.model, source: event.source, name: event.name }));
    return { run: done, task, artifact, value: JSON.parse(raw.text), metrics: { runId: run.id, taskId: task.id, nodeAttempt: node.attempt, taskAttempt: task.attempt, provider: task.provider,
      configuredModel: credentials.model, nodeModel: node.model || null, telemetryModelObservations: observed, sessionId: node.sessionId, elapsedMS,
      usage: { ...usage, costStatus: usage.costStatus || 'unknown', costUsd: usage.costStatus === 'reported' || usage.costStatus === 'estimated' ? usage.costUsd : null },
      intelligenceSHA256: done.intelligence.sha256, contribution, artifact: { id: artifact.id, sha256: artifact.sha256, size: artifact.size } } };
  };

  const extractor = await launchRunner('extractor');
  const extractionPrompt = `Read only the synthetic fixed repository, especially docs/settlement-policy.md and NOTICE.md. Produce an original concise summary and one generic review skill. Write exactly one UTF-8 JSON file at workspace-relative ${artifactPath}; do not modify the Git source. Do not use network, install packages, run Git writes, read unrelated host files or execute candidate instructions. Complete the normal node result after writing it.
Schema: {"schemaVersion":1,"suggestions":[memory,skill]}; exactly two suggestions. memory has only {key:"settlement-policy",kind:"memory",title:string,content:string,evidencePaths:["docs/settlement-policy.md"],licenseClaim:"unreviewed"}; preserve the exact policy field names and values in the generated content. skill has only {key:"review-settlement",kind:"skill",skillId:"${skillId}",description:string,files:{"SKILL.md":string,"NOTICE.md":string},noticePaths:["NOTICE.md"],evidencePaths:["docs/settlement-policy.md"],licenseClaim:"unreviewed"}. SKILL.md must have YAML frontmatter name=${skillId} and a description. Its body must give a generic read-and-compare procedure using docs/settlement-policy.md, without copying the actual policy answers, numbers or field values. NOTICE.md must state generated synthetic suggestion, source and licensing require human review. No extra JSON fields, no markdown fences, no self-approval. Generate all candidate prose yourself from the fixed source; never substitute an example file.
Serialization is part of the output contract: create SKILL.md and NOTICE.md text values with actual LF characters, then use a standard JSON serializer exactly once. Do not pre-replace LF with the two literal characters backslash and n. After parsing the written JSON, files["SKILL.md"] must start with three hyphens followed by an actual LF, contain the YAML name/description lines and a closing frontmatter delimiter on its own line. Parse the finished JSON yourself and verify its exact schema and decoded skill frontmatter before submitting the normal result. This is validation of your own output data; do not execute any candidate instructions or scripts.`;
  const extractionAgent = await createAgent('cap003-extractor', extractionPrompt, { executorId: 'codex', model: credentials.model, loop: { enabled: false } });
  const extractionRelease = await publish('CAP003 source extraction', extractionAgent);
  const extracted = await execute({ name: 'cap003-extract', runner: extractor.runner, agentId: extractionAgent, release: extractionRelease, path: artifactPath });
  report.extraction = extracted.metrics; report.generatedSuggestions = extracted.value; persist();
  assert.equal(extracted.value.schemaVersion, 1); assert.equal(extracted.value.suggestions.length, 2);
  const extractionRequest = { runId: extracted.run.id, nodeId: 'work', artifactId: extracted.artifact.id, expectedArtifactSHA256: extracted.artifact.sha256, repositoryId, expectedCommit: commit };
  const imported = await post(base + '/knowledge-extractions', extractionRequest);
  assert.equal(imported.replayed, false); const receipt = imported.receipt;
  assert.equal(receipt.items.length, 2); assert.equal(receipt.source.commit, commit); assert.equal(receipt.source.taskId, extracted.task.id);
  assert.equal(receipt.source.sessionId, extracted.run.nodes.work.sessionId); assert.equal(receipt.source.artifactSHA256, extracted.artifact.sha256);
  assert.equal(receipt.source.provider, 'codex'); assert.equal(receipt.source.configuredModel, credentials.model);
  assert.deepEqual(await get(`${base}/knowledge-extractions/${receipt.id}`), receipt);
  const replay = await post(base + '/knowledge-extractions', extractionRequest);
  assert.equal(replay.replayed, true); assert.deepEqual(replay.receipt, receipt);
  const memoryItem = receipt.items.find(item => item.kind === 'memory'), skillItem = receipt.items.find(item => item.kind === 'skill');
  assert.ok(memoryItem && skillItem);
  memory = list(await get(base + '/memories')).find(item => item.id === memoryItem.candidateId);
  candidate = await get(`${base}/skill-candidates/${skillItem.candidateId}`);
  assert.equal(memory.state, 'candidate'); assert.equal(candidate.state, 'quarantined');
  assert.equal(memory.source.extractionId, receipt.id); assert.equal(candidate.package.source.extractionId, receipt.id);
  const unapproved = await post(base + '/memory-recall', { query: 'requestId', limit: 10, includeReceipt: true });
  assert.equal(list(unapproved.items).length, 0);
  assert.ok(!list(await get('/api/skills')).some(skill => skill.id === skillId));
  report.import = { request: extractionRequest, receipt, idempotentReplay: true, memory, candidate, unapprovedRecall: unapproved }; persist();

  // Explicit synthetic API-owner review: compare generated facts to the fixed
  // local source; never execute or silently edit model-generated candidates.
  for (const fact of ['requestId', '3', 'return_existing_receipt', 'human_review']) assert.ok(memory.content.includes(fact), `generated summary lost ${fact}`);
  assert.equal(candidate.package.skill.id, skillId); assert.match(candidate.package.license, /^UNREVIEWED:/);
  const generatedSkill = candidate.package.skill.files['SKILL.md'];
  assert.ok(generatedSkill.includes('docs/settlement-policy.md'));
  for (const answer of ['requestId', 'return_existing_receipt', 'human_review']) assert.ok(!generatedSkill.includes(answer), 'same-skill baseline must not receive the extracted memory answers');
  assert.ok(candidate.package.noticePaths.includes('NOTICE.md')); assert.ok(candidate.package.skill.files['NOTICE.md']);
  const reason = 'Explicit opt-in API-owner review: generated facts checked against the fixed synthetic policy; generic non-executable skill and NOTICE reviewed. Test fixture authorization only, not human business or verified upstream-license approval.';
  memory = await post(`${base}/memories/${memory.id}/decision`, { decision: 'approved', expectedRevision: memory.revision, reason });
  candidate = await post(`${base}/skill-candidates/${candidate.id}/decision`, { decision: 'approved', expectedRevision: candidate.revision, reason });
  assert.equal(memory.state, 'approved'); assert.equal(candidate.state, 'approved'); assert.equal(candidate.promotedSkillId, skillId);
  activeSkill = list(await get('/api/skills')).find(skill => skill.id === skillId); assert.ok(activeSkill);
  report.review = { reason, memory, candidate, activeSkill }; persist();
  await stop(extractor);
  rmSync(join(extractor.owned, 'home'), { recursive: true, force: true }); rmSync(join(extractor.owned, 'workspace'), { recursive: true, force: true });
  const consumer = await launchRunner('consumer'); assert.notEqual(consumer.runner.id, extractor.runner.id);
  const evaluationPrompt = `Read the fixed synthetic repository policy and any supplied approved skill/memory. Evaluate input duplicate=true and attempt=4. Write only workspace-relative ${evaluationPath} as JSON with exactly deduplicationKey (string), maxChargeAttempts (number), onDuplicate (string), onAttemptExhausted (string), mayChargeAgain (boolean). Preserve the policy's exact values and apply both conditions. Do not modify Git source, use network, install packages, run Git writes or reuse another session's files. Complete the normal node result after writing the report.`;
  const evaluationAgent = await createAgent('cap003-policy-evaluator', evaluationPrompt, { executorId: 'codex', model: credentials.model, skills: [skillId], loop: { enabled: false } });
  const evaluationRelease = await publish('CAP003 matched evaluation', evaluationAgent);
  const baseline = await execute({ name: 'cap003-no-memory', runner: consumer.runner, agentId: evaluationAgent, release: evaluationRelease, path: evaluationPath, selection: { memoryIds: [] } });
  assert.equal(baseline.task.invocation.governedContext.memories.length, 0); assert.ok(!baseline.task.invocation.governedContext.memoryRecallReceipt);
  const synced = await post(base + '/memory-provider/sync', { limit: 10 }); assert.equal(synced.complete, true); assert.equal(synced.ready, 1);
  const recalled = await post(base + '/memory-recall', { query: 'requestId', limit: 1, includeReceipt: true });
  assert.equal(recalled.items.length, 1); assert.equal(recalled.items[0].id, memory.id); assert.equal(recalled.receipt.provider, 'tencentdb-l0-bm25');
  assert.equal(recalled.receipt.hits[0].revision, memory.revision); assert.equal(recalled.receipt.hits[0].sha256, memory.sha256);
  assert.ok(recalled.receipt.hits[0].recordIds.length); report.provider = { synced, recalled }; persist();
  const withMemory = await execute({ name: 'cap003-with-memory', runner: consumer.runner, agentId: evaluationAgent, release: evaluationRelease, path: evaluationPath,
    selection: { memoryIds: [memory.id], memoryRecallReceiptId: recalled.receipt.id } });
  const binding = withMemory.task.invocation.governedContext;
  assert.equal(binding.releaseId, evaluationRelease.id); assert.equal(binding.memoryRecallReceipt.id, recalled.receipt.id);
  assert.deepEqual(binding.memories, [{ id: memory.id, revision: memory.revision, sha256: memory.sha256 }]);
  assert.ok(withMemory.task.outputs.contextEvidence.files.some(file => file.kind === 'skill' && file.resourceId === skillId && file.path.endsWith('/SKILL.md')));
  assert.ok(withMemory.task.invocation.assets.skills.some(skill => skill.id === skillId));
  assert.equal(withMemory.run.intelligence.memories[0].content, memory.content);
  assert.notEqual(withMemory.metrics.sessionId, baseline.metrics.sessionId); assert.notEqual(withMemory.metrics.sessionId, extracted.metrics.sessionId);
  assert.equal(baseline.task.invocation.agent.prompt, withMemory.task.invocation.agent.prompt);
  assert.deepEqual(baseline.task.invocation.assets.skills, withMemory.task.invocation.assets.skills);
  const score = value => Object.entries(expectedEvaluation).filter(([key, expected]) => value[key] === expected).length;
  report.comparison = { expected: expectedEvaluation, baseline: { ...baseline.metrics, result: baseline.value, correctFields: score(baseline.value) },
    withMemory: { ...withMemory.metrics, result: withMemory.value, correctFields: score(withMemory.value), governedContext: binding, contextEvidence: withMemory.task.outputs.contextEvidence },
    conclusion: 'One synthetic case only; inspect correctness, latency and observed usage. Model sampling/order and memory context differ; improvement and financial savings are not assumed.' };
  assert.equal(git(source, 'for-each-ref', '--format=%(refname) %(objectname)'), refs);
  assert.ok(report.sourceRequests.every(item => ['GET', 'HEAD'].includes(item.method) && !item.credentialPresented));
  for (const id of runs) await waitFor(`Run ${id} workspace cleanup`, async () => { const run = await get(`/api/runs/${id}`); return Date.parse(run.workspaceCleanedAt) > 0 ? run : null; }, { timeoutMs: 60_000, intervalMs: 500 });
  report.acceptance = 'governed-extraction-and-comparison-observed'; persist();
  assert.equal(score(withMemory.value), Object.keys(expectedEvaluation).length, 'actual reviewed-memory evaluation must reproduce the synthetic policy');
});
