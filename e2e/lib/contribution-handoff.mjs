// Opt-in real OS/network handoff. Docker Linux is a separate OS/process and
// network namespace on this host, not a separate physical/cloud failure domain.
import assert from 'node:assert/strict';
import { spawn, execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { createServer } from 'node:http';
import { createReadStream, chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { networkInterfaces, tmpdir } from 'node:os';
import { join } from 'node:path';
import { api, get, post, put, del, list, waitFor, ensureProject, cleanup, track } from './api.mjs';
import { cfg, uid } from './env.mjs';
import { createWorkflow, script, approval, trackRun } from './fixtures.mjs';

const baseSHA = '75fbd59a82073c0fe4fa24b92ac976888b0970f6';
const publicSource = 'https://github.com/huasheng718/superagent-foundation.git';
const helperSource = readFileSync(new URL(import.meta.url));
const hash = value => createHash('sha256').update(value).digest('hex');
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const dockerDirectEnv = ['HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'http_proxy', 'https_proxy', 'all_proxy'].flatMap(key => ['--env', key + '=']).concat(['--env', 'NO_PROXY=*', '--env', 'no_proxy=*']);
const git = (dir, ...args) => execFileSync('git', ['-C', dir, ...args], {
  encoding: 'utf8', env: { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null', GIT_TERMINAL_PROMPT: '0' },
}).trim();

function writerScript(platform) {
  return `set -eu\npython3 - <<'PY'\n` + String.raw`import json, os, pathlib, platform, subprocess
root = pathlib.Path.cwd().resolve()
repositories = ([root] if (root / '.git').exists() else []) + [p.parent for p in root.glob('*/.git')]
assert len(repositories) == 1, str(repositories)
repo = repositories[0].resolve()
def git(*args): return subprocess.check_output(['git', '-C', str(repo), *args], text=True).strip()
kind = ` + JSON.stringify(platform) + `
assert platform.system() == ('Darwin' if kind == 'mac' else 'Linux')
assert git('rev-parse', 'HEAD') == '${baseSHA}'
own = repo / 'contributions' / (kind + '_feature.py')
peer = repo / 'contributions' / (('linux' if kind == 'mac' else 'mac') + '_feature.py')
assert not peer.exists(), 'peer contribution leaked into independent checkout'
own.parent.mkdir(parents=True, exist_ok=True)
own.write_text('def identity():\\n    return "' + kind + '"\\n')
compile(own.read_text(), str(own), 'exec')
scope = {}; exec(compile(own.read_text(), str(own), 'exec'), scope)
assert scope['identity']() == kind
home = pathlib.Path.home().resolve()
result = {'platform': platform.system(), 'machine': platform.machine(), 'pid': os.getpid(), 'hostname': platform.node(),
          'home': str(home), 'workspace': str(root), 'repository': str(repo), 'baseSHA': git('rev-parse', 'HEAD'),
          'gitDir': git('rev-parse', '--absolute-git-dir'), 'gitCommonDir': git('rev-parse', '--git-common-dir'),
          'worktrees': git('worktree', 'list', '--porcelain'), 'ownPath': str(own.relative_to(repo)), 'ownContent': own.read_text(),
          'peerVisible': peer.exists(), 'gitStatusBeforeExport': git('status', '--porcelain'), 'testPassed': True,
          'hostCredentialsPresent': any(os.environ.get(k) for k in ('CODEX_API_KEY','OPENAI_API_KEY','ANTHROPIC_API_KEY','AGENTFLOW_GIT_TOKEN'))}
assert not result['hostCredentialsPresent'], 'writer inherited an unrelated host credential'
(root / 'reports').mkdir(exist_ok=True)
(root / 'reports' / 'writer.json').write_text(json.dumps(result, indent=2))
print(json.dumps(result), flush=True)
PY`;
}

// Test-only receiving processes consume bytes, never a writer mount or an
// object cache. Production reconstruction remains the existing Git helper.
const readerScript = String.raw`import hashlib, json, os, pathlib, platform, subprocess, sys, tempfile
metadata = json.loads(sys.argv[1])
raw = sys.stdin.buffer.read(16 * 1024 * 1024 + 1)
assert len(raw) == metadata['size'] and hashlib.sha256(raw).hexdigest() == metadata['sha256']
with tempfile.TemporaryDirectory(prefix='fresh-contribution-reader-') as temp:
    root = pathlib.Path(temp)
    home = root / 'home'; home.mkdir(mode=0o700)
    repo = root / 'repository'; repo.mkdir()
    bundle = root / 'aggregate.bundle'; bundle.write_bytes(raw); bundle.chmod(0o600)
    env = {'PATH': os.environ['PATH'], 'HOME': str(home), 'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': '/dev/null',
           'GIT_TERMINAL_PROMPT': '0', 'GIT_NO_REPLACE_OBJECTS': '1', 'GIT_ALLOW_PROTOCOL': 'file', 'LC_ALL': 'C'}
    def git(*args):
        return subprocess.check_output(['git', '-C', str(repo), '-c', 'core.hooksPath=/dev/null', '-c', 'protocol.allow=never', '-c', 'protocol.file.allow=always', *args], env=env, text=True, stderr=subprocess.STDOUT).strip()
    git('init', '-q')
    verification = git('bundle', 'verify', str(bundle))
    advertisement = git('bundle', 'list-heads', str(bundle))
    assert advertisement == metadata['resultSHA'] + ' refs/heads/agentflow-contribution', advertisement
    git('fetch', '-q', str(bundle), 'refs/heads/agentflow-contribution:refs/heads/reconstructed')
    git('checkout', '-q', '--detach', metadata['resultSHA'])
    assert git('rev-parse', 'HEAD') == metadata['resultSHA']
    git('merge-base', '--is-ancestor', metadata['baseSHA'], metadata['resultSHA'])
    changed = git('diff', '--name-only', metadata['baseSHA'], metadata['resultSHA']).splitlines()
    assert changed == ['contributions/linux_feature.py', 'contributions/mac_feature.py'], changed
    assert hashlib.sha256((repo / 'README.md').read_bytes()).hexdigest() == metadata['baselineReadmeSHA256']
    files = {}
    for kind in ['mac', 'linux']:
        path = repo / 'contributions' / (kind + '_feature.py')
        source = path.read_text(); scope = {}; exec(compile(source, str(path), 'exec'), scope)
        assert scope['identity']() == kind
        files[str(path.relative_to(repo))] = hashlib.sha256(path.read_bytes()).hexdigest()
    assert git('status', '--porcelain') == ''
    print(json.dumps({'platform': platform.system(), 'machine': platform.machine(), 'resultSHA': metadata['resultSHA'],
                      'baseSHA': metadata['baseSHA'], 'bundleSHA256': metadata['sha256'], 'bundleSize': len(raw),
                      'changedFiles': changed, 'fileSHA256': files, 'baselinePreserved': True, 'cleanWorkingTree': True,
                      'bundleVerification': verification, 'gatePassed': True, 'gateResultSHA': metadata['resultSHA']}), flush=True)
`;

export async function runContributionHandoff(t, { phase = "full", evidenceDir, entrySource, entryName = "40-contribution-handoff.test.mjs", repositoryMetadata = {}, sensitiveValues = [], sensitiveEndpoints = [], onIntegrated, onWritersReady } = {}) {
  assert.ok(['receipts', 'full'].includes(phase));
  await ensureProject();
  const productRoot = git(process.cwd(), 'rev-parse', '--show-toplevel');
  const dir = mkdtempSync(join(tmpdir(), 'superagent-contribution-handoff-'));
  const evidence = evidenceDir || process.env.AGENTFLOW_E2E_HANDOFF_EVIDENCE_DIR || join(dir, 'evidence');
  mkdirSync(evidence, { recursive: true });
  const secrets = new Set([cfg.token, cfg.tokenB, ...sensitiveValues].filter(Boolean));
  const localEndpoints = new Set(sensitiveEndpoints);
  const report = { startedAt: new Date().toISOString(), phase, acceptanceLevel: 'incomplete', gatePassed: false,
    publicSource, baseSHA, productBaseSHA: git(process.cwd(), 'rev-parse', 'HEAD'),
    harnessSHA256: hash(entrySource), helperSHA256: hash(helperSource), productTrackedDiffSHA256: hash(execFileSync('git', ['diff', 'HEAD'])),
    boundaries: { scope: 'base contribution handoff; extension runs report their own boundaries', physicalDevices: 1, linux: 'local Docker OS/process/network namespace', modelUsed: false,
      authority: 'one existing Engine; two independent Runs', sourceAccess: 'read-only HTTP; no hosting-write credential',
      publication: 'not attempted', controllerIntegration: phase === 'full' ? 'required' : 'pending separate full phase' },
    runners: [], runs: [], sourceRequests: [], findings: [],
  };
  const logs = { mac: '', linux: '' };
  const readers = [], readerContainers = [];
  const sessions = new Map();
  const beforeCleanup = [], afterCleanup = [];
  const previousDefault = (await get('/api/runners/default')).runnerId || '';
  let native, linux, sourceServer;
  const container = uid('handoff-linux');
  const redact = value => {
    let out = typeof value === 'string' ? value : JSON.stringify(value, (key, item) => /(?:apiKey|authToken|sessionToken|gitToken|password|secret)/i.test(key) ? '[REDACTED]' : ['remoteAddress', 'hostname'].includes(key) ? '[experiment host]' : item, 2);
    for (const secret of secrets) out = out.replaceAll(secret, '[REDACTED]');
    for (const endpoint of localEndpoints) out = out.replaceAll(endpoint, '[local source endpoint]');
    return out;
  };
  const persist = () => {
    writeFileSync(join(evidence, 'handoff-facts.json'), redact(report) + '\n');
    for (const [kind, log] of Object.entries(logs)) writeFileSync(join(evidence, `${kind}-runner.log`), redact(log));
    writeFileSync(join(evidence, entryName), entrySource);
    writeFileSync(join(evidence, 'contribution-handoff.mjs'), helperSource);
  };
  t.after(async () => {
    for (const fn of beforeCleanup) { try { await fn(); } catch (error) { report.findings.push("extension snapshot: " + error.message); } }
    // Snapshot authoritative failure/success before cancellation/deletion.
    for (const item of report.runs) {
      try {
        item.finalRun = await get(`/api/runs/${item.runId}`);
        item.finalTasks = list(await get('/api/runners/tasks')).filter(task => task.runId === item.runId);
        item.events = [];
        const token = sessions.get(item.runnerId);
        for (const task of item.finalTasks) {
          if (token) item.events.push({ taskId: task.id, response: await get(`/api/runners/tasks/${task.id}/events`, { auth: false, headers: { Authorization: `Bearer ${token}` } }) });
        }
      } catch (error) { item.snapshotError = error.message; }
    }
    persist();
    await cleanup();
    for (const fn of afterCleanup) { try { await fn(); } catch (error) { report.findings.push("extension cleanup: " + error.message); } }
    await put('/api/runners/default', { runnerId: previousDefault }, { expect: [200, 404] }).catch(() => {});
    native?.kill('SIGTERM');
    if (native && native.exitCode === null && native.signalCode === null) await Promise.race([new Promise(resolve => native.once('exit', resolve)), delay(5_000)]);
    if (native && native.exitCode === null && native.signalCode === null) native.kill('SIGKILL');
    if (linux) {
      try { execFileSync('docker', ['stop', '--time', '5', container], { stdio: 'pipe' }); } catch { /* experiment may already be stopped */ }
      try { execFileSync('docker', ['rm', '-f', container], { stdio: 'pipe' }); } catch { /* --rm already removed it */ }
    }
    for (const proc of readers) if (proc.exitCode === null && proc.signalCode === null) proc.kill('SIGKILL');
    for (const name of readerContainers) { try { execFileSync('docker', ['rm', '-f', name], { stdio: 'pipe' }); } catch { /* --rm removed a completed reader */ } }
    for (const token of sessions.values()) await api('POST', '/runners/deregister', {}, { auth: false, headers: { Authorization: `Bearer ${token}` }, expect: [200, 401] }).catch(() => {});
    sourceServer?.closeAllConnections();
    if (sourceServer) await new Promise(resolve => sourceServer.close(resolve));
    for (const name of ['native-state.json', 'agentflow-gate-mcp', 'agentflow-runner-mac', 'agentflow-runner-linux', 'native-home', 'native-workspace', 'native-tmp', 'native-reader-home', 'native-reader-tmp']) rmSync(join(dir, name), { recursive: true, force: true });
    report.finishedAt = new Date().toISOString();
    report.cleanup = { nativeProcessStopped: !native || native.exitCode !== null || native.signalCode !== null,
      linuxContainerRemoved: (() => { try { execFileSync('docker', ['inspect', container], { stdio: 'pipe' }); return false; } catch { return true; } })(),
      sourceServerStopped: !sourceServer?.listening, privateSessionFilesRemoved: !existsSync(join(dir, 'native-state.json')) };
    persist();
    t.diagnostic(`contribution handoff evidence: ${evidence}`);
  });

  // Copy exactly the already-fetched public initial ref; never expose the
  // product worktree, its private config, unrelated refs or dirty source.
  assert.equal(git(process.cwd(), 'rev-parse', 'refs/remotes/superagent/main'), baseSHA);
  const initialBundle = join(dir, 'public-initial.bundle');
  git(process.cwd(), 'bundle', 'create', initialBundle, 'refs/remotes/superagent/main');
  const source = join(dir, 'source.git'); mkdirSync(source);
  git(source, 'init', '--bare', '-q');
  git(source, 'fetch', '-q', initialBundle, 'refs/remotes/superagent/main:refs/heads/main');
  git(source, 'symbolic-ref', 'HEAD', 'refs/heads/main');
  git(source, 'update-server-info');
  const refsBefore = git(source, 'for-each-ref', '--format=%(refname) %(objectname)');
  assert.equal(refsBefore, `refs/heads/main ${baseSHA}`);
  report.baselineReadmeSHA256 = hash(execFileSync('git', ['-C', source, 'show', `${baseSHA}:README.md`]));
  const addresses = Object.entries(networkInterfaces()).flatMap(([name, items]) => items.filter(a => a.family === 'IPv4' && !a.internal).map(a => ({ name, address: a.address })));
  const address = process.env.AGENTFLOW_E2E_HANDOFF_SOURCE_ADDRESS || addresses.find(a => /^en\d+$/.test(a.name))?.address;
  assert.ok(address && addresses.some(a => a.address === address), 'use an actual host address reachable from Docker');
  sourceServer = createServer((req, res) => {
    let relative;
    try { relative = decodeURIComponent(new URL(req.url, 'http://fixture.invalid').pathname).replace(/^\/source\.git/, ''); } catch { res.writeHead(400).end(); return; }
    const entry = { method: req.method, path: relative, remoteAddress: req.socket.remoteAddress, credentialPresented: Boolean(req.headers.authorization), status: 0 };
    report.sourceRequests.push(entry);
    const reply = status => { entry.status = status; res.writeHead(status).end(); };
    if (!['GET', 'HEAD'].includes(req.method)) { reply(405); return; }
    if (entry.credentialPresented || !/^\/(?:HEAD|info\/refs|objects\/(?:info\/packs|[0-9a-f]{2}\/[0-9a-f]{38}|pack\/pack-[0-9a-f]{40}\.(?:idx|pack)))$/.test(relative)) { reply(404); return; }
    const path = join(source, relative.slice(1));
    if (!existsSync(path) || !statSync(path).isFile()) { reply(404); return; }
    entry.status = 200; res.writeHead(200, { 'Content-Type': 'application/octet-stream', 'Content-Length': statSync(path).size, 'Cache-Control': 'no-store' });
    if (req.method === 'HEAD') res.end(); else createReadStream(path).pipe(res);
  });
  await new Promise(resolve => sourceServer.listen(0, '0.0.0.0', resolve));
  const sourcePort = sourceServer.address().port;
  const sourceURL = `http://${address}:${sourcePort}/source.git`;
  localEndpoints.add(sourceURL); localEndpoints.add(address);
  report.sourceMirror = { url: sourceURL, refsBefore, initialBundleSHA256: hash(readFileSync(initialBundle)), mountSharedWithRunners: false,
    routing: { mac: 'direct host HTTP', linux: 'direct HTTP through Docker bridge to the same host fixture', frozenURLIdentical: true, experimentContainerProxiesCleared: true } };
  assert.equal((await fetch(sourceURL + '/HEAD')).status, 200);
  const dockerProbe = spawn('docker', ['run', '--rm', '--pull=never', ...dockerDirectEnv, 'node:22', 'python3', '-c', 'import urllib.request,sys; print(urllib.request.urlopen(sys.argv[1],timeout=10).read().decode())', sourceURL + '/HEAD'], { stdio: ['ignore', 'pipe', 'pipe'] });
  let probeOut = ''; for (const stream of [dockerProbe.stdout, dockerProbe.stderr]) stream.on('data', data => { probeOut += data; });
  assert.equal(await new Promise(resolve => dockerProbe.once('exit', resolve)), 0, `Docker read-only HTTP reachability: ${probeOut}`);
  report.sourceMirror.dockerProbe = probeOut.trim();

  const nativeBinary = join(dir, 'agentflow-runner-mac');
  const res = await fetch(`${cfg.base}/runners/agentflow-runner?os=darwin&arch=${process.arch === 'arm64' ? 'arm64' : 'amd64'}`, { signal: AbortSignal.timeout(120_000) });
  assert.equal(res.status, 200); writeFileSync(nativeBinary, Buffer.from(await res.arrayBuffer())); chmodSync(nativeBinary, 0o700);
  const linuxBinary = join(dir, 'agentflow-runner-linux');
  execFileSync('go', ['build', '-o', linuxBinary, './cmd/agentflow-runner'], { cwd: productRoot, env: { ...process.env, CGO_ENABLED: '0', GOOS: 'linux', GOARCH: 'arm64' }, stdio: 'pipe' });
  chmodSync(linuxBinary, 0o755);
  report.binaries = { mac: { sha256: hash(readFileSync(nativeBinary)), os: 'darwin', arch: process.arch }, linux: { sha256: hash(readFileSync(linuxBinary)), os: 'linux', arch: 'arm64' } };
  report.linuxImage = { name: 'node:22', id: execFileSync('docker', ['image', 'inspect', 'node:22', '--format', '{{.Id}}'], { encoding: 'utf8' }).trim() };

  const names = { mac: uid('handoff-mac'), linux: uid('handoff-linux') };
  for (const kind of ['mac', 'linux']) {
    const { code } = await post('/api/runners/bind-codes', {}); secrets.add(code);
    let proc;
    if (kind === 'mac') {
      for (const name of ['native-home', 'native-tmp']) mkdirSync(join(dir, name), { mode: 0o700 });
      // A workflow Agent needs the packaged gate tool even with no user MCPs.
      const gateBinary = join(dir, 'agentflow-gate-mcp');
      execFileSync('go', ['build', '-o', gateBinary, './cmd/agentflow-gate-mcp'], { cwd: productRoot, stdio: 'pipe' });
      proc = native = spawn(nativeBinary, ['--server', cfg.base, '--name', names.mac, '--state', join(dir, 'native-state.json'), '--workspace', join(dir, 'native-workspace'), '--heartbeat', '2s', '--auto-update=false'], {
        cwd: dir, env: { PATH: process.env.PATH, HOME: join(dir, 'native-home'), TMPDIR: join(dir, 'native-tmp'), LANG: 'en_US.UTF-8', AGENTFLOW_BROWSER: 'off', AGENTFLOW_GATE_MCP_BINARY: gateBinary, AGENTFLOW_RUNNER_BOOTSTRAP_TOKEN: code }, stdio: ['ignore', 'pipe', 'pipe'],
      });
    } else {
      const targetBase = `http://host.docker.internal:${new URL(cfg.base).port}`;
      const command = 'mkdir -p /runner/home /runner/tmp /runner/workspace; read -r AGENTFLOW_RUNNER_BOOTSTRAP_TOKEN; export AGENTFLOW_RUNNER_BOOTSTRAP_TOKEN; exec /usr/local/bin/agentflow-runner "$@"';
      proc = linux = spawn('docker', ['run', '--rm', '--pull=never', '-i', '--name', container,
        '--mount', `type=bind,source=${linuxBinary},target=/usr/local/bin/agentflow-runner,readonly`,
        '--env', 'HOME=/runner/home', '--env', 'TMPDIR=/runner/tmp', '--env', 'AGENTFLOW_BROWSER=off', ...dockerDirectEnv,
        'node:22', 'sh', '-c', command, 'handoff-runner', '--server', targetBase, '--name', names.linux,
        '--state', '/runner/state.json', '--workspace', '/runner/workspace', '--heartbeat', '2s', '--auto-update=false'], { stdio: ['pipe', 'pipe', 'pipe'] });
      proc.stdin.end(code + '\n');
    }
    for (const stream of [proc.stdout, proc.stderr]) stream.on('data', data => { logs[kind] += data; });
    proc.on('error', error => { logs[kind] += '\nspawn: ' + error.message; });
    const runner = await waitFor(`${kind} Runner online`, async () => list(await get('/api/runners')).find(r => r.name === names[kind] && r.status === 'online'), { timeoutMs: 45_000, intervalMs: 500 });
    assert.equal(runner.os, kind === 'mac' ? 'darwin' : 'linux');
    const state = JSON.parse(kind === 'mac' ? readFileSync(join(dir, 'native-state.json'), 'utf8') : execFileSync('docker', ['exec', container, 'cat', '/runner/state.json'], { encoding: 'utf8' }));
    assert.ok(state.sessionToken); secrets.add(state.sessionToken); sessions.set(runner.id, state.sessionToken);
    const record = { kind, id: runner.id, os: runner.os, arch: runner.arch, version: runner.runnerVersion, home: kind === 'mac' ? join(dir, 'native-home') : '/runner/home' };
    if (kind === 'linux') {
      record.mounts = JSON.parse(execFileSync('docker', ['inspect', container, '--format', '{{json .Mounts}}'], { encoding: 'utf8' }));
      assert.equal(record.mounts.length, 1); assert.equal(record.mounts[0].Destination, '/usr/local/bin/agentflow-runner'); assert.equal(record.mounts[0].RW, false);
      const proxyProbe = spawn('docker', ['exec', container, 'python3', '-c', 'import urllib.request,sys; print(urllib.request.urlopen(sys.argv[1],timeout=10).read().decode())', sourceURL + '/HEAD'], { stdio: ['ignore', 'pipe', 'pipe'] });
      let output = ''; for (const stream of [proxyProbe.stdout, proxyProbe.stderr]) stream.on('data', data => { output += data; });
      assert.equal(await new Promise(resolve => proxyProbe.once('exit', resolve)), 0, `Linux direct source HTTP: ${output}`);
      record.sourceHTTPProbe = output.trim();
    }
    report.runners.push(record); persist();
  }

  const repositoryId = uid('handoff-public-source');
  track(`repository ${repositoryId}`, () => del(`/api/repositories?id=${repositoryId}`));
  await post('/api/repositories', { id: repositoryId, projectId: cfg.projectId, provider: 'gongfeng', name: repositoryId, gongfengProjectId: `e2e-auto/${repositoryId}`, cloneUrl: sourceURL, defaultBranch: 'main', ...repositoryMetadata });
  report.repositoryId = repositoryId;
  if (onWritersReady) await onWritersReady({ report, repositoryId, baseSHA, sourceURL, persist, sessions, onBeforeCleanup: fn => beforeCleanup.push(fn) });
  for (const runner of report.runners) {
    await put('/api/runners/default', { runnerId: runner.id });
    const workflowId = await createWorkflow(`handoff-${runner.kind}`, [script('writer', writerScript(runner.kind), {
      artifactOutputs: [{ name: 'writer', path: 'reports/writer.json', mediaType: 'application/json', required: true }],
    }), approval('hold', '')], { sandboxProfileId: 'local' });
    const issue = await post('/api/issues', { projectId: cfg.projectId, title: `E2E contribution ${runner.kind} ${cfg.runTag}`, description: 'public fixed baseline; immutable contribution only; no push', priority: 'medium', kind: 'requirement', status: 'todo', workflowId, repositoryIds: [repositoryId] });
    track(`issue ${issue.id}`, () => post(`/api/issues/${issue.id}/close`, {}, { expect: [200, 404, 409] }));
    await post(`/api/issues/${issue.id}/dispatch`, { workflowId, runnerWorkspaceLayout: 'attempt', workspaceBaseSHAs: { [repositoryId]: baseSHA } });
    const summary = await waitFor('bound Run created', async () => list(await get(`/api/issues/${issue.id}/runs`))[0], { timeoutMs: 60_000, intervalMs: 300 });
    const runId = summary.id || summary.runId; trackRun(runId);
    const item = { kind: runner.kind, runId, runnerId: runner.id, repositoryId, baseSHA }; report.runs.push(item); persist();
    const done = await waitFor(`${runner.kind} accepted contribution`, async () => {
      item.observedRun = await get(`/api/runs/${runId}`);
      item.observedTasks = list(await get('/api/runners/tasks')).filter(task => task.runId === runId); persist();
      assert.equal(item.observedRun.executionContext?.executionRunnerId, runner.id, 'Run binding must freeze before default changes');
      const task = item.observedTasks.find(task => task.nodeId === 'writer');
      if (['failed', 'cancelled', 'rejected'].includes(item.observedRun.status) || task?.status === 'failed') throw new Error(`${runner.kind} writer failed: ${task?.error || item.observedRun.error}`);
      return task?.status === 'success' && item.observedRun.nodes?.hold?.status === 'waiting_approval' ? task : null;
    }, { timeoutMs: 120_000, intervalMs: 500 });
    assert.equal(done.runnerId || done.leaseOwner, runner.id);
    assert.equal(done.contributionSpecs?.length, 1, 'Engine must freeze contribution metadata');
    assert.equal(done.acceptedContributions?.length, 1, 'successful writer must have accepted immutable contribution');
    const receipt = done.acceptedContributions[0];
    assert.equal(receipt.taskId, done.id); assert.equal(receipt.attempt, done.attempt); assert.equal(receipt.repositoryId, repositoryId); assert.equal(receipt.baseSHA, baseSHA);
    assert.match(receipt.resultSHA, /^[0-9a-f]{40}$/); assert.notEqual(receipt.resultSHA, baseSHA); assert.match(receipt.sha256, /^[0-9a-f]{64}$/); assert.ok(receipt.acceptedAt);
    item.taskId = done.id; item.attempt = done.attempt; item.receipt = receipt;
    const artifact = item.observedRun.artifacts?.find(a => a.name === 'writer'); assert.ok(artifact, 'writer environment facts artifact');
    item.writerFacts = JSON.parse((await api('GET', `/api/runs/${runId}/artifacts/${encodeURIComponent(artifact.id)}`, undefined, { raw: true })).text);
    assert.equal(item.writerFacts.baseSHA, baseSHA); assert.equal(item.writerFacts.peerVisible, false); assert.equal(item.writerFacts.hostCredentialsPresent, false); assert.equal(item.writerFacts.testPassed, true);
    persist();
  }
  assert.notEqual(report.runs[0].runId, report.runs[1].runId);
  assert.notEqual(report.runs[0].runnerId, report.runs[1].runnerId);
  assert.notEqual(report.runs[0].receipt.resultSHA, report.runs[1].receipt.resultSHA);
  assert.notEqual(report.runs[0].writerFacts.home, report.runs[1].writerFacts.home);
  assert.notEqual(report.runs[0].writerFacts.gitDir, report.runs[1].writerFacts.gitDir);
  assert.equal(git(source, 'for-each-ref', '--format=%(refname) %(objectname)'), refsBefore, 'read-only source refs changed');
  assert.ok(report.sourceRequests.every(r => ['GET', 'HEAD'].includes(r.method) && !r.credentialPresented), 'no source write or credential request');
  if (phase === 'full') {
    for (const item of report.runs) {
      await post(`/api/runs/${item.runId}/approve`, { nodeId: 'hold', context: 'Automated P0 contribution transport test authorization; not human business acceptance.' });
      item.successfulRun = await waitFor('source Run terminal success', async () => {
        const run = await get(`/api/runs/${item.runId}`);
        if (['failed', 'cancelled', 'rejected'].includes(run.status)) throw new Error('source Run failed after test hold authorization');
        return run.status === 'succeeded' ? run : null;
      }, { timeoutMs: 60_000, intervalMs: 500 });
    }
    // Controller integration must survive without either writer's live files,
    // process, HOME or the read-only input fixture service.
    native.kill('SIGTERM');
    if (native.exitCode === null && native.signalCode === null) await Promise.race([new Promise(resolve => native.once('exit', resolve)), delay(5_000)]);
    if (native.exitCode === null && native.signalCode === null) native.kill('SIGKILL');
    execFileSync('docker', ['stop', '--time', '5', container], { stdio: 'pipe' });
    rmSync(join(dir, 'native-workspace'), { recursive: true, force: true });
    sourceServer.closeAllConnections(); await new Promise(resolve => sourceServer.close(resolve));
    rmSync(source, { recursive: true, force: true });
    report.writerAvailabilityAtIntegration = { macProcessStopped: native.exitCode !== null || native.signalCode !== null,
      macWorkspaceRemoved: !existsSync(join(dir, 'native-workspace')), sourceServerStopped: !sourceServer.listening,
      linuxContainerRemoved: (() => { try { execFileSync('docker', ['inspect', container], { stdio: 'pipe' }); return false; } catch { return true; } })() };
    assert.ok(Object.values(report.writerAvailabilityAtIntegration).every(Boolean));
    report.integration = await post(`/api/runs/${report.runs[0].runId}/contributions/integrate`, { repositoryId, taskIds: report.runs.map(r => r.taskId) }, { timeoutMs: 120_000 });
    assert.equal(report.integration.repositoryId, repositoryId); assert.equal(report.integration.baseSHA, baseSHA);
    assert.match(report.integration.resultSHA, /^[0-9a-f]{40}$/); assert.match(report.integration.manifestSHA256, /^[0-9a-f]{64}$/);
    assert.equal(report.integration.inputs?.length, 2); assert.ok(report.runs.every(r => report.integration.inputs.some(i => i.taskId === r.taskId && i.resultSHA === r.receipt.resultSHA)));
    assert.equal(report.integration.state, 'prepared');
    assert.equal(hash(Buffer.from(JSON.stringify({ ...report.integration, id: '', manifestSHA256: '' }))), report.integration.manifestSHA256, 'manifest must hash exact immutable metadata');
    const duplicate = await post(`/api/runs/${report.runs[0].runId}/contributions/integrate`, { repositoryId, taskIds: report.runs.map(r => r.taskId) }, { timeoutMs: 120_000 });
    assert.equal(duplicate.id, report.integration.id); assert.equal(duplicate.resultSHA, report.integration.resultSHA); assert.equal(duplicate.bundleSHA256, report.integration.bundleSHA256);
    report.duplicateIntegration = { id: duplicate.id, resultSHA: duplicate.resultSHA, bundleSHA256: duplicate.bundleSHA256 };
    report.unavailableContributionDenial = (await api('POST', `/api/runs/${report.runs[0].runId}/contributions/integrate`, { repositoryId, taskIds: [report.runs[0].taskId, 'missing-contribution-task'] }, { expect: 409, raw: true })).status;
    report.nonAdminDenial = (await api('POST', `/api/runs/${report.runs[0].runId}/contributions/integrate`, { repositoryId, taskIds: report.runs.map(r => r.taskId) }, { token: cfg.tokenB, expect: 403, raw: true })).status;
    report.runnerDenial = (await api('POST', `/api/runs/${report.runs[0].runId}/contributions/integrate`, { repositoryId, taskIds: report.runs.map(r => r.taskId) }, { auth: false, headers: { Authorization: `Bearer ${sessions.get(report.runs[0].runnerId)}` }, expect: [401, 403], raw: true })).status;
    const download = await fetch(`${cfg.base}/api/runs/${report.runs[0].runId}/contributions/${report.integration.id}/content`, { headers: { Authorization: `Bearer ${cfg.token}` }, signal: AbortSignal.timeout(120_000) });
    assert.equal(download.status, 200); assert.equal(download.headers.get('content-type')?.split(';')[0], 'application/x-git-bundle');
    const aggregate = Buffer.from(await download.arrayBuffer());
    assert.equal(aggregate.length, report.integration.bundleSize); assert.equal(hash(aggregate), report.integration.bundleSHA256);
    writeFileSync(join(evidence, 'controller-aggregate.bundle'), aggregate, { mode: 0o600 });
    writeFileSync(join(evidence, 'controller-integration.json'), redact(report.integration) + '\n');
    const metadata = { resultSHA: report.integration.resultSHA, baseSHA, sha256: report.integration.bundleSHA256, size: aggregate.length, baselineReadmeSHA256: report.baselineReadmeSHA256 };
    report.receivingGates = [];
    for (const kind of ['mac', 'linux']) {
      let proc;
      if (kind === 'mac') {
        for (const name of ['native-reader-home', 'native-reader-tmp']) mkdirSync(join(dir, name), { mode: 0o700 });
        proc = spawn('python3', ['-c', readerScript, JSON.stringify(metadata)], { env: { PATH: process.env.PATH, HOME: join(dir, 'native-reader-home'), TMPDIR: join(dir, 'native-reader-tmp') }, stdio: ['pipe', 'pipe', 'pipe'] });
      } else {
        const name = uid('handoff-independent-reader'); readerContainers.push(name);
        proc = spawn('docker', ['run', '--rm', '--pull=never', '--network', 'none', '--name', name, '-i', ...dockerDirectEnv, 'node:22', 'python3', '-c', readerScript, JSON.stringify(metadata)], { stdio: ['pipe', 'pipe', 'pipe'] });
      }
      readers.push(proc);
      let stdout = '', stderr = '';
      proc.stdout.on('data', data => { stdout += data; }); proc.stderr.on('data', data => { stderr += data; });
      const timer = setTimeout(() => proc.kill('SIGKILL'), 60_000);
      proc.stdin.end(aggregate);
      const code = await new Promise(resolve => proc.once('exit', resolve)); clearTimeout(timer);
      assert.equal(code, 0, `${kind} fresh immutable receiving gate: ${stderr} ${stdout}`);
      const gate = JSON.parse(stdout); assert.equal(gate.resultSHA, report.integration.resultSHA); assert.equal(gate.gatePassed, true); assert.equal(gate.platform, kind === 'mac' ? 'Darwin' : 'Linux');
      report.receivingGates.push({ ...gate, source: 'controller HTTP bytes only; private empty receiving repository', readerNetwork: kind === 'linux' ? 'none' : 'Git file protocol only', mounts: [] });
      persist();
    }
    report.acceptanceLevel = 'local_mac_linux_controller_reconstruction_and_fixed_sha_gates'; report.gatePassed = true;
    if (onIntegrated) await onIntegrated({
      t, report, evidence, dir, nativeBinary, sourceURL, repositoryId, aggregate, persist,
      addSecret: value => { if (value) secrets.add(value); },
      addEndpoint: value => { if (value) localEndpoints.add(value); },
      onBeforeCleanup: fn => beforeCleanup.push(fn), onAfterCleanup: fn => afterCleanup.push(fn),
      restoreReadOnlySource: async () => {
        assert.equal(sourceServer.listening, false);
        mkdirSync(source); git(source, 'init', '--bare', '-q');
        git(source, 'fetch', '-q', initialBundle, 'refs/remotes/superagent/main:refs/heads/main');
        git(source, 'fetch', '-q', join(evidence, 'controller-aggregate.bundle'), 'refs/heads/agentflow-contribution:refs/heads/gated-aggregate');
        git(source, 'symbolic-ref', 'HEAD', 'refs/heads/main'); git(source, 'update-server-info');
        await new Promise(resolve => sourceServer.listen(sourcePort, '0.0.0.0', resolve));
        report.sourceMirror.refsForGateRun = git(source, 'for-each-ref', '--format=%(refname) %(objectname)');
        assert.equal(git(source, 'rev-parse', 'refs/heads/main'), baseSHA);
      },
    });
  } else {
    report.acceptanceLevel = 'accepted_receipts_only';
  }
  persist();

  return report;
}
