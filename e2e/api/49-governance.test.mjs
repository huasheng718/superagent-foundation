// Deterministic governance acceptance on a test-owned project. No model,
// external memory service or hosted Git publication is invoked.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { api, get, post, put, del, list, track, cleanup, tokenOwner } from '../lib/api.mjs';
import { cfg, uid } from '../lib/env.mjs';

const projectId = uid('governance');
const base = `/api/projects/${projectId}`;
const match = value => ({ headers: { 'If-Match': JSON.stringify(value.sourceRevision) } });
const reason = 'Disposable acceptance fixture; no business approval implied';

before(async () => {
  const owner = await tokenOwner();
  await post('/api/projects', { id: projectId, name: `E2E governance ${cfg.runTag}`, ownerId: owner.id });
  track(`project ${projectId}`, () => del(base));
});
after(cleanup);

function trackSource(kind, id) {
  track(`${kind} ${id}`, async () => {
    const value = list(await get(`/api/${kind}`)).find(item => item.id === id);
    if (value) await del(`/api/${kind}/${id}`, match(value));
  });
}

function trackRelease(release) {
  track(`release ${release.id}`, async () => {
    const current = await get(`${base}/team-releases/${release.id}`);
    if (current.state === 'published') await post(`${base}/team-releases/${release.id}/revoke`, { expectedRevision: current.revision, reason });
  });
}

test('source policy requires a current revision and keeps inspection available', async () => {
  for (const kind of ['rules', 'skills', 'agents', 'mcp-servers']) {
    const id = uid(kind);
    trackSource(kind, id);
    const source = await put(`/api/${kind}`, {
      id, projectId, name: `E2E ${kind}`, content: 'Reviewed fixture', prompt: 'Reviewed fixture',
      ...(kind === 'agents' ? { executorId: cfg.executor } : {}),
      ...(kind === 'mcp-servers' ? { scope: 'project', transport: 'http', url: 'https://example.invalid/mcp' } : {}),
    }, { headers: { 'If-None-Match': '*' } });
    const path = `/api/${kind}/${id}/distribution`;
    const input = { policy: { copy: 'deny', edit: 'allow', export: 'deny' }, reason };
    await put(path, input, { expect: 428 });
    const decision = await put(path, input, match(source));
    await put(path, input, { ...match(source), expect: 409 });
    const inspected = list(await get(`/api/${kind}`)).find(item => item.id === id);
    assert.equal(inspected.name, source.name, 'operation policy must not hide source inspection');
    assert.equal(inspected.sourceRevision, decision.sourceRevision);
    await put(path, { policy: { copy: 'allow', edit: 'allow', export: 'allow' }, reason }, match(decision));
  }
});

test('release, memory and skill review lifecycles keep immutable history and reject stale decisions', async () => {
  const ruleId = uid('frozen-rule');
  trackSource('rules', ruleId);
  const rule = await put('/api/rules', { id: ruleId, projectId, name: ruleId, content: 'REVIEWED_V1' }, { headers: { 'If-None-Match': '*' } });
  assert.equal(list(await get(`${base}/team-releases`)).length, 0);
  const release = await post(`${base}/team-releases`, { name: 'E2E reviewed release', expectedBaseVersion: 0, ruleIds: [ruleId] });
  trackRelease(release);
  assert.equal(release.version, 1);
  const frozen = await get(`${base}/team-releases/${release.id}`);
  await put('/api/rules', { ...rule, content: 'MUTABLE_V2' }, match(rule));
  assert.equal((await get(`${base}/team-releases/${release.id}`)).sha256, frozen.sha256);

  let memory = await post(`${base}/memories`, { title: 'E2E proof', content: 'governanceproof reviewed evidence', source: { path: 'e2e/api/49-governance.test.mjs' } });
  assert.equal(memory.state, 'candidate');
  assert.deepEqual(list(await post(`${base}/memory-recall`, { query: 'governanceproof' })), []);
  const decision = { decision: 'approved', expectedRevision: memory.revision, reason };
  memory = await post(`${base}/memories/${memory.id}/decision`, decision);
  track(`memory ${memory.id}`, async () => {
    const current = list(await get(`${base}/memories`)).find(item => item.id === memory.id);
    if (current?.state === 'approved') await post(`${base}/memories/${memory.id}/decision`, { decision: 'revoked', expectedRevision: current.revision, reason });
  });
  await post(`${base}/memories/${memory.id}/decision`, decision, { expect: 409 });
  assert.ok(list(await post(`${base}/memory-recall`, { query: 'governanceproof' })).some(item => item.id === memory.id));
  const context = await post(`${base}/context/resolve`, { releaseId: release.id, memoryIds: [memory.id] });
  assert.equal(context.releaseSha256, frozen.sha256);
  assert.equal(context.configuration.assets.rules[0].content, 'REVIEWED_V1');
  assert.equal(context.memories[0].sha256, memory.sha256);
  await post(`${base}/memories/${memory.id}/decision`, { decision: 'revoked', expectedRevision: memory.revision, reason });
  assert.deepEqual(list(await post(`${base}/memory-recall`, { query: 'governanceproof' })), []);
  await post(`${base}/context/resolve`, { releaseId: release.id, memoryIds: [memory.id] }, { expect: 404 });

  // A locally authored release has no verified Git import; export/update
  // previews must reject it before attempting any repository access.
  for (const action of ['git-export-preview', 'git-update-preview']) {
    const result = await post(`${base}/team-releases/${release.id}/${action}`, { expectedReleaseSha256: release.sha256 }, { expect: 400 });
    assert.ok(result.error);
  }
  await post(`${base}/team-releases/${release.id}/revoke`, { expectedRevision: release.revision, reason });
  const restored = await post(`${base}/team-releases/${release.id}/restore`, { name: 'E2E restored history', expectedBaseVersion: 1, reason });
  trackRelease(restored);
  assert.equal(restored.version, 2);
  assert.notEqual(restored.id, release.id);
  assert.deepEqual(restored.payload, frozen.payload);
  assert.equal((await get(`${base}/team-releases/${release.id}`)).state, 'revoked');
  assert.ok(Array.isArray(await get(`${base}/default-upgrades`)));

  const skillId = uid('reviewed-skill');
  trackSource('skills', skillId);
  const candidate = await post(`${base}/skill-candidates`, {
    skill: { id: skillId, name: 'E2E reviewed skill', files: { 'SKILL.md': `---\nname: ${skillId}\ndescription: Disposable reviewed fixture\n---\n\n# Evidence\n`, LICENSE: 'MIT fixture\n' } },
    publisher: 'E2E fixture', license: 'MIT', noticePaths: ['LICENSE'], source: { path: 'e2e/api/49-governance.test.mjs' },
  });
  assert.equal(candidate.state, 'quarantined');
  assert.ok(!list(await get('/api/skills')).some(item => item.id === skillId));
  assert.ok(list(await get(`${base}/skill-candidates`)).some(item => item.id === candidate.id));
  assert.equal((await get(`${base}/skill-candidates/${candidate.id}`)).sha256, candidate.sha256);
  const approve = { decision: 'approved', expectedRevision: candidate.revision, reason };
  const approved = await post(`${base}/skill-candidates/${candidate.id}/decision`, approve);
  assert.equal(approved.promotedSkillId, skillId);
  assert.ok(list(await get('/api/skills')).some(item => item.id === skillId));
  await post(`${base}/skill-candidates/${candidate.id}/decision`, approve, { expect: 409 });
});

test('governance rejects incomplete publication, import, extraction and caller-forged memory scope', async () => {
  assert.ok(Array.isArray(await get(`${base}/team-git-proposals`)));
  await get(`${base}/team-git-proposals/missing`, { expect: 404 });
  await post(`${base}/team-git-proposals`, {}, { expect: 400 });
  for (const action of ['decision', 'cancel', 'publish', 'reconcile']) {
    await post(`${base}/team-git-proposals/missing/${action}`, {}, { expect: action === 'publish' || action === 'reconcile' ? [403, 428] : 428 });
  }
  for (const action of ['preview', 'publish']) {
    await post(`${base}/team-imports/${action}`, { undocumentedField: true }, { expect: 400 });
  }
  const extraction = await post(`${base}/knowledge-extractions`, {}, { expect: 400 });
  assert.equal(extraction.code, 'extraction_invalid');
  await get(`${base}/knowledge-extractions/missing`, { expect: 404 });
  const sync = await post(`${base}/memory-provider/sync`, { limit: 1 }, { expect: 400, headers: { 'X-TDAI-Project': 'forged-project' } });
  assert.match(sync.error, /scope headers are forbidden/);
});

test('a test-owned archived project can be restored without changing ownership', async () => {
  const original = await get(base);
  assert.ok((await del(base)).archivedAt);
  const restored = await post(`${base}/restore`, {});
  assert.equal(restored.ownerId, original.ownerId);
  assert.ok(!restored.archivedAt);
});
