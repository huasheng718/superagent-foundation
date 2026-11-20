// Explicit local API-only scope acceptance. Root owns services, restart,
// outage/storage experiments and final cleanup. No daemon/CLI/model is invoked.
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createHash, randomBytes } from 'node:crypto';
import { lstatSync, mkdirSync, readFileSync, readdirSync, realpathSync, statSync, writeFileSync } from 'node:fs';
import { join, relative } from 'node:path';

const enabled = process.env.AGENTFLOW_E2E_MEMORY_SCOPE === '1';
const base = (process.env.AGENTFLOW_E2E_BASE || 'http://127.0.0.1:18292').replace(/\/+$/, '');
const hash = value => createHash('sha256').update(value).digest('hex');
const label = 'shared-session-scope47';

test('CAP003 actual auth-required project/user scope, review, warm recall and native expiry', {
  skip: !enabled && 'requires root-owned isolated API/private provider and synthetic token fixtures', timeout: 120_000,
}, async t => {
  const url = new URL(base);
  assert.ok(['127.0.0.1', 'localhost'].includes(url.hostname) && url.port === '18292', 'only the owned local 18292 fixture is supported');
  const tokenFile = process.env.AGENTFLOW_E2E_SCOPE_TOKEN_FILE;
  const evidence = process.env.AGENTFLOW_E2E_SCOPE_EVIDENCE_DIR;
  assert.ok(tokenFile && evidence, 'explicit synthetic token file and owned evidence directory are required');
  assert.equal(statSync(tokenFile).mode & 0o077, 0);
  const tokens = JSON.parse(readFileSync(tokenFile, 'utf8'));
  for (const actor of ['admin', 'alice', 'bob', 'outsider', 'revoked']) assert.ok(typeof tokens[actor] === 'string' && tokens[actor].length > 8);
  assert.equal(new Set(Object.values(tokens)).size, 5, 'synthetic actor credentials must be distinct');
  const userIds = JSON.parse(process.env.AGENTFLOW_E2E_SCOPE_USER_IDS || '{}');
  for (const actor of ['admin', 'alice', 'bob', 'outsider']) assert.ok(Number.isSafeInteger(userIds[actor]) && userIds[actor] > 0);
  assert.equal(new Set(Object.values(userIds)).size, 4);
  mkdirSync(evidence, { recursive: true, mode: 0o700 });
  const report = { startedAt: new Date().toISOString(), acceptance: 'incomplete', base, userIds,
    harnessSHA256: hash(readFileSync(new URL(import.meta.url))), calls: [], projects: {}, storage: [], checks: [],
    boundaries: { modelInvocations: 0, daemonOrCLIStarted: false, hostCredentialsRead: false, identity: 'only root-provided synthetic API tokens',
      metadata: 'PublicProject metadata is intentionally public to authenticated users; governed content has separate ACL',
      memoryAPI: 'list/create/decision/recall/context only; unsupported detail/edit/delete/export/receipt-GET are not ACL evidence',
      measurement: 'API JSON bytes and optional stat logical file sizes; not MySQL OCTET_LENGTH, allocation, secure erasure or billing',
      outerWork: 'root owns same-binding restart, provider outage/replacement, SQL storage measurements and complete cleanup' } };
  const sanitize = value => {
    let raw = JSON.stringify(value, (key, item) => /apiKey|authorization|password|secret|credential|bearer/i.test(key) ? '[REDACTED]' : item, 2);
    for (const token of Object.values(tokens)) raw = raw.replaceAll(token, '[REDACTED]');
    return raw;
  };
  const persist = () => {
    writeFileSync(join(evidence, 'scope-facts.json'), sanitize(report) + '\n', { mode: 0o600 });
    writeFileSync(join(evidence, '47-memory-scope-storage.test.mjs'), readFileSync(new URL(import.meta.url)), { mode: 0o600 });
  };
  t.after(() => { report.finishedAt = new Date().toISOString(); persist(); t.diagnostic(`CAP003 API scope evidence: ${evidence}`); });
  const call = async (actor, method, path, body, expected = [200, 201], headers = {}) => {
    const started = performance.now();
    const auth = actor === 'invalid' ? 'afp_scope47_invalid_token' : tokens[actor];
    const response = await fetch(base + path, { method, headers: { Accept: 'application/json', ...(auth ? { Authorization: `Bearer ${auth}` } : {}),
      ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}), ...headers },
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}), signal: AbortSignal.timeout(8_000), redirect: 'error' });
    const text = await response.text(); assert.ok(Buffer.byteLength(text) <= 1 << 20);
    let data; try { data = JSON.parse(text); } catch { data = text; }
    report.calls.push({ actor: actor || 'anonymous', method, path, status: response.status, latencyMS: Number((performance.now() - started).toFixed(3)),
      responseBytes: Buffer.byteLength(text), requestBody: body === undefined ? undefined : structuredClone(body), response: structuredClone(data) }); persist();
    assert.ok(expected.includes(response.status), `${actor || 'anonymous'} ${method} ${path}: unexpected HTTP ${response.status}`);
    return data;
  };
  const route = item => `/api/projects/${item.id}`;
  const recall = (actor, item, query = label) => call(actor, 'POST', route(item) + '/memory-recall', { query, limit: 10, includeReceipt: true });
  const sync = (actor, item) => call(actor, 'POST', route(item) + '/memory-provider/sync', { limit: 10 });
  const decide = (actor, item, memory, decision) => call(actor, 'POST', `${route(item)}/memories/${memory.id}/decision`, { decision, expectedRevision: memory.revision, reason: 'Explicit synthetic API-owner scope acceptance; not human business approval.' });
  const contextBody = (item, memory = item.memory, receipt = item.receipt) => ({ releaseId: item.release.id, memoryIds: [memory.id], memoryRecallReceiptId: receipt.id });
  const context = (actor, item, body = contextBody(item), expected = [200]) => call(actor, 'POST', route(item) + '/context/resolve', body, expected);
  const providerStat = () => {
    const configured = process.env.AGENTFLOW_E2E_SCOPE_PROVIDER_DATA_ROOT;
    if (!configured) return { observed: false };
    const root = realpathSync(configured);
    assert.ok(root !== '/' && root !== process.env.HOME && root.endsWith('/data'), 'stat only the explicit owned provider data directory');
    const files = []; let visited = 0;
    const walk = (dir, depth = 0) => {
      assert.ok(depth <= 12 && visited++ <= 10_000, 'bounded provider data inventory');
      for (const entry of readdirSync(dir, { withFileTypes: true })) {
        const path = join(dir, entry.name);
        if (entry.isDirectory() && !['node_modules', 'MemoryCore', 'src', 'logs', '.git'].includes(entry.name)) walk(path, depth + 1);
        else if (entry.isFile() && /\.(?:sqlite3?|db|jsonl)(?:-(?:wal|shm))?$/.test(entry.name)) {
          const stat = lstatSync(path); if (stat.isFile()) files.push({ path: relative(root, path), bytes: stat.size });
        }
      }
    };
    walk(root); return { observed: true, files, logicalBytes: files.reduce((sum, file) => sum + file.bytes, 0), contentsRead: false };
  };
  const storage = async phase => {
    const native = {};
    for (const actor of ['alice', 'bob']) {
      const item = report.projects[actor];
      const memories = await call(actor, 'GET', route(item) + '/memories');
      const candidates = await call(actor, 'GET', route(item) + '/skill-candidates');
      native[actor] = { memoryCount: memories.length, states: memories.map(m => ({ id: m.id, state: m.state, revision: m.revision })),
        memoryAPIJSONBytes: Buffer.byteLength(JSON.stringify(memories)), skillCandidateAPIJSONBytes: Buffer.byteLength(JSON.stringify(candidates)) };
    }
    report.storage.push({ phase, observedAt: new Date().toISOString(), native, provider: providerStat() }); persist();
  };

  const auth = await call(null, 'GET', '/api/auth/current'); assert.equal(auth.authRequired, true); assert.equal(auth.user.isGuest, true);
  assert.deepEqual(await call(null, 'GET', '/api/auth/local'), { enabled: false });
  await call(null, 'POST', '/api/auth/local', {}, [403], { Origin: base });
  for (const actor of [null, 'invalid', 'revoked']) await call(actor, 'GET', '/api/projects', undefined, [401]);
  const users = await call('admin', 'GET', '/api/users');
  const admins = await call('admin', 'GET', '/api/platform/admins');
  for (const actor of ['alice', 'bob', 'outsider']) {
    const user = users.find(item => item.id === userIds[actor]); assert.ok(user);
    assert.notEqual(user.platformRole, 'admin'); assert.ok(!admins.some(item => item.loginName === user.loginName));
  }
  report.checks.push('actual auth required; local-login disabled; anonymous/invalid/revoked tokens rejected; Alice/Bob/outsider non-admin');
  const tag = Date.now().toString(36) + '-' + randomBytes(3).toString('hex');
  for (const actor of ['alice', 'bob']) {
    const item = await call('admin', 'POST', '/api/projects', { id: `scope47-${tag}-${actor}`, key: `S47${actor.toUpperCase()}`, name: `Synthetic ${label}`, ownerId: userIds[actor], members: [] });
    assert.equal(item.ownerId, userIds[actor]); report.projects[actor] = item;
    assert.equal((await call(actor, 'GET', route(item))).id, item.id);
  }
  const alice = report.projects.alice, bob = report.projects.bob;
  const setAliceMembers = members => call('alice', 'PUT', '/api/projects', {
    id: alice.id, name: alice.name, key: alice.key, ownerId: alice.ownerId, members,
  });
  // Observe intended public metadata separately; it is never private memory.
  assert.equal((await call('bob', 'GET', route(alice))).id, alice.id);
  await call('bob', 'PUT', '/api/projects', { ...alice, name: 'forged project edit' }, [403]);
  await storage('empty');
  for (const actor of ['alice', 'bob']) {
    const item = report.projects[actor], sentinel = `SCOPE47_${actor.toUpperCase()}_${tag}`;
    item.sentinel = sentinel;
    const body = { title: label, content: `${label}: ${sentinel}. Synthetic reviewed scope fixture.`, source: { path: 'synthetic/shared-session-scope47.md' } };
    item.memory = await call(actor, 'POST', route(item) + '/memories', body); assert.equal(item.memory.state, 'candidate');
    item.rejected = await call(actor, 'POST', route(item) + '/memories', { ...body, content: `${label}: REJECTED_${sentinel}` });
    item.skillId = `scope47-${actor}-${randomBytes(4).toString('hex')}`;
    item.candidate = await call(actor, 'POST', route(item) + '/skill-candidates', { skill: { id: item.skillId, name: label, description: 'Synthetic read-only scope fixture.',
      content: `---\nname: ${item.skillId}\ndescription: Synthetic read-only scope fixture.\n---\n\n${label}: ${sentinel}\n`, files: { 'NOTICE.md': 'Synthetic acceptance fixture; not production knowledge.\n' } },
      source: { path: 'synthetic/shared-session-scope47.md' }, publisher: 'synthetic API owner', license: 'Synthetic fixture only; no upstream material.', noticePaths: ['NOTICE.md'] });
    assert.equal(item.candidate.state, 'quarantined');
    assert.equal((await recall(actor, item)).items.length, 0);
  }
  for (const [actor, foreign] of [['alice', bob], ['bob', alice], ['outsider', alice], ['outsider', bob]]) {
    await call(actor, 'GET', route(foreign) + '/memories', undefined, [403]);
    await call(actor, 'POST', route(foreign) + '/memories', { title: label, content: 'forged', source: { path: 'synthetic/shared-session-scope47.md' } }, [403]);
    await call(actor, 'POST', `${route(foreign)}/memories/${foreign.memory.id}/decision`, { decision: 'approved', expectedRevision: foreign.memory.revision, reason: 'unauthorized fixture probe' }, [403]);
    await call(actor, 'GET', `${route(foreign)}/skill-candidates/${foreign.candidate.id}`, undefined, [403]);
    await call(actor, 'POST', `${route(foreign)}/skill-candidates/${foreign.candidate.id}/decision`, { decision: 'approved', expectedRevision: foreign.candidate.revision, reason: 'unauthorized fixture probe' }, [403]);
    await call(actor, 'POST', route(foreign) + '/memory-recall', { query: label, limit: 10, includeReceipt: true }, [403]);
  }
  for (const [actor, own, foreign] of [['alice', alice, bob], ['bob', bob, alice]]) {
    await call(actor, 'GET', `${route(own)}/skill-candidates/${foreign.candidate.id}`, undefined, [404]); // supported scoped detail route
    await call(actor, 'POST', `${route(own)}/memories/${foreign.memory.id}/decision`, { decision: 'approved', expectedRevision: foreign.memory.revision, reason: 'foreign ID in own scope' }, [404]);
    for (const field of ['projectId', 'userId', 'teamId', 'agentId', 'sessionId', 'scope', 'bucket']) {
      await call(actor, 'POST', route(own) + '/memory-recall', { query: label, includeReceipt: true, [field]: field === 'userId' ? userIds.outsider : foreign.id }, [400]);
    }
    for (const header of ['x-tdai-user-id', 'x-tdai-agent-id', 'x-tdai-service-id', 'x-tdai-project-id']) {
      await call(actor, 'POST', route(own) + '/memory-recall', { query: label, includeReceipt: true }, [400], { [header]: foreign.id });
    }
    await call(actor, 'POST', route(own) + '/memories', { title: label, content: 'forged state', source: { path: 'synthetic/shared-session-scope47.md' }, state: 'approved' }, [400]);
    own.rejected = await decide(actor, own, own.rejected, 'rejected');
    own.memory = await decide(actor, own, own.memory, 'approved');
    own.candidate = await call(actor, 'POST', `${route(own)}/skill-candidates/${own.candidate.id}/decision`, { decision: 'approved', expectedRevision: own.candidate.revision, reason: 'Explicit synthetic fixture owner review; no execution.' });
    assert.equal(own.memory.state, 'approved'); assert.equal(own.candidate.state, 'approved');
    own.release = await call(actor, 'POST', route(own) + '/team-releases', { name: label, expectedBaseVersion: 0, agentIds: [], skillIds: [own.skillId] });
    assert.equal(own.release.state, 'published'); assert.equal(own.release.payload.assets.skills[0].id, own.skillId);
    await call(actor, 'POST', `${route(own)}/memories/${own.memory.id}/decision`, { decision: 'revoked', expectedRevision: 1, reason: 'stale reviewer' }, [409]);
    const synced = await sync(actor, own); assert.equal(synced.complete, true); assert.equal(synced.ready, 1);
    const recalled = await recall(actor, own); assert.equal(recalled.items.length, 1); assert.equal(recalled.items[0].id, own.memory.id);
    assert.equal(recalled.items[0].content, own.memory.content); assert.ok(!JSON.stringify(recalled).includes(foreign.sentinel));
    own.receipt = recalled.receipt;
    assert.equal(own.receipt.provider, 'tencentdb-l0-bm25'); assert.equal(own.receipt.scope.projectId, own.id); assert.equal(own.receipt.scope.userId, userIds[actor]);
    assert.equal(own.receipt.hits[0].revision, own.memory.revision); assert.equal(own.receipt.hits[0].sha256, own.memory.sha256);
    own.context = await context(actor, own);
    for (let i = 0; i < 2; i++) assert.equal((await recall(actor, own)).items[0].id, own.memory.id);
  }
  await storage('two-approved-owner-mirrors');
  await context('alice', alice, contextBody(alice, alice.memory, bob.receipt), [404]);
  await context('bob', bob, contextBody(bob, bob.memory, alice.receipt), [404]);
  await context('alice', alice, { ...contextBody(alice), memoryIds: [bob.memory.id] }, [404]);
  await context('bob', bob, { ...contextBody(bob), releaseId: alice.release.id }, [404]);
  report.checks.push('project ACL and own-route foreign IDs denied; forged scope rejected; candidate/rejected invisible; each approved native pin matches real BM25; warmed calls remain isolated');

  // Same project, distinct authenticated users: no earlier project/Memory error
  // can mask the receipt check. Both users first resolve their own valid pin.
  await setAliceMembers([{ userId: userIds.bob, role: 'member' }]);
  assert.ok((await call('bob', 'GET', route(alice) + '/memories')).some(m => m.id === alice.memory.id));
  await call('bob', 'POST', `${route(alice)}/memories/${alice.memory.id}/decision`, { decision: 'revoked', expectedRevision: alice.memory.revision, reason: 'member is not owner' }, [403]);
  const unsynced = await call('bob', 'POST', route(alice) + '/memory-recall', { query: label, includeReceipt: true }, [503]);
  assert.equal(unsynced.code, 'memory_provider_sync_required'); // own user mirror has not been synced
  assert.equal((await sync('bob', alice)).complete, true);
  const bobInAlice = await recall('bob', alice); assert.equal(bobInAlice.items[0].id, alice.memory.id); assert.equal(bobInAlice.receipt.scope.userId, userIds.bob);
  await context('bob', alice, contextBody(alice, alice.memory, bobInAlice.receipt));
  await context('bob', alice, contextBody(alice, alice.memory, alice.receipt), [404]);
  await context('alice', alice, contextBody(alice, alice.memory, bobInAlice.receipt), [404]);
  await setAliceMembers([]);
  await call('bob', 'POST', route(alice) + '/memory-recall', { query: label, includeReceipt: true }, [403]);
  await setAliceMembers([{ userId: userIds.bob, role: 'member' }]);
  assert.equal((await recall('bob', alice)).items[0].id, alice.memory.id);
  report.checks.push('same-project member may read but not decide; own-user receipt valid, other-user receipt denied; removal denies warmed recall without deleting the existing mirror');

  const expiresAt = new Date(Date.now() + 10_000).toISOString();
  const expiryQuery = 'expiryneedle' + randomBytes(5).toString('hex');
  let expiring = await call('alice', 'POST', route(alice) + '/memories', { title: expiryQuery, content: `${expiryQuery} synthetic pin`, source: { path: 'synthetic/shared-session-scope47.md' }, expiresAt });
  expiring = await decide('alice', alice, expiring, 'approved'); await sync('alice', alice);
  const expiryRecall = await recall('alice', alice, expiryQuery); assert.equal(expiryRecall.items.length, 1); assert.equal(expiryRecall.items[0].id, expiring.id);
  await context('alice', alice, contextBody(alice, expiring, expiryRecall.receipt));
  await new Promise(resolveTimer => setTimeout(resolveTimer, Math.max(0, Date.parse(expiresAt) - Date.now() + 100)));
  assert.equal((await recall('alice', alice, expiryQuery)).items.length, 0);
  await context('alice', alice, contextBody(alice, expiring, expiryRecall.receipt), [404]);
  assert.equal((await sync('alice', alice)).complete, true);
  alice.memory = await decide('alice', alice, alice.memory, 'revoked');
  assert.equal((await recall('alice', alice)).items.length, 0);
  assert.equal((await recall('bob', alice)).items.length, 0);
  await context('alice', alice, contextBody(alice), [404]);
  assert.equal((await sync('alice', alice)).complete, true); assert.equal((await sync('bob', alice)).complete, true);
  await setAliceMembers([]);
  const bobAfter = await recall('bob', bob); assert.equal(bobAfter.items[0].id, bob.memory.id);
  assert.equal((await context('bob', bob)).sha256, bob.context.sha256);
  await storage('alice-revoked-and-owned-mirrors-deleted-bob-kept');
  report.checks.push('real short-lived memory expires and its prior pin is denied; revoked warmed Alice memory disappears for both user scopes; unaffected Bob original receipt resolves unchanged');
  report.handoff = { aliceProjectId: alice.id, bobProjectId: bob.id, bobMemory: { id: bob.memory.id, revision: bob.memory.revision, sha256: bob.memory.sha256 },
    bobReleaseId: bob.release.id, bobReceiptId: bob.receipt.id, bobContextSHA256: bob.context.sha256,
    projectsAndAuditRetainedForRoot: true, rootMustPerformRestartOutageAndPhysicalCleanup: true };
  report.httpOperationCount = report.calls.length;
  report.logicalContentBytes = { aliceMemory: Buffer.byteLength(alice.memory.content), bobMemory: Buffer.byteLength(bob.memory.content) };
  report.acceptance = 'api-scope-lifecycle-receipt-and-native-expiry-passed'; persist();
});
