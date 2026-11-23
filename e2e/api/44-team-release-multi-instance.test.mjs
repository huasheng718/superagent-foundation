// Opt-in disposable local MySQL acceptance across two distinct API processes.
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { randomUUID, createHash } from 'node:crypto';
import { writeFileSync } from 'node:fs';

const bases = [process.env.AGENTFLOW_E2E_BASE, process.env.AGENTFLOW_E2E_SECOND_BASE];
const enabled = process.env.AGENTFLOW_E2E_RELEASE_MULTI_INSTANCE === '1';
const loopback = value => { try { const u = new URL(value); return u.protocol === 'http:' && u.hostname === '127.0.0.1'; } catch { return false; } };
const digest = value => createHash('sha256').update(JSON.stringify(value)).digest('hex');

test('CAP001 two independent HTTP instances serialize same-base publication and restore', {
  skip: !enabled && 'requires explicit disposable local two-instance harness', timeout: 60_000,
}, async () => {
  assert.equal(process.env.AGENTFLOW_E2E_LOCAL_MYSQL_CONTAINER, 'superagent-p0-contribution');
  assert.ok(bases.every(loopback));
  assert.notEqual(bases[0], bases[1]);
  const pids = [process.env.AGENTFLOW_E2E_PRIMARY_PID, process.env.AGENTFLOW_E2E_SECOND_PID].map(Number);
  assert.ok(pids.every(pid => Number.isInteger(pid) && pid > 1));
  assert.notEqual(pids[0], pids[1]);
  const token = process.env.AGENTFLOW_E2E_TOKEN;
  assert.ok(token);
  const id = `e2e-auto-multi-release-${randomUUID()}`;
  const path = '/api/projects/e2e-auto/team-releases';
  const report = { acceptance: 'running', apiBases: bases, processIds: pids, ruleId: id, cleanup: {}, boundaries: { realHTTPProcesses: true, sharedMySQL: true, hostedWrites: false, modelUsed: false, immutableAuditRowsRetained: true } };
  const persist = () => { if (process.env.AGENTFLOW_E2E_RELEASE_REPORT) writeFileSync(process.env.AGENTFLOW_E2E_RELEASE_REPORT, JSON.stringify(report, null, 2) + '\n', { mode: 0o600 }); };
  const request = async (base, method, endpoint, body, expected, headers = {}) => {
    const response = await fetch(base + endpoint, { method, headers: { Authorization: `Bearer ${token}`, ...headers, ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) }, ...(body === undefined ? {} : { body: JSON.stringify(body) }), signal: AbortSignal.timeout(15_000) });
    const text = await response.text();
    let value; try { value = JSON.parse(text); } catch { value = null; }
    if (expected !== undefined) assert.equal(response.status, expected, `${method} ${endpoint}: ${text.slice(0, 300)}`);
    return { status: response.status, value };
  };
  const call = async (method, endpoint, body, expected = 200, headers = {}) => (await request(bases[0], method, endpoint, body, expected, headers)).value;
  let savedRule;
  const owned = [];
  try {
    for (const [index, base] of bases.entries()) {
      const health = await request(base, 'GET', '/api/health', undefined, 200);
      assert.equal(health.value.pid, pids[index], 'health must attest the distinct process PID');
      await request(base, 'GET', '/api/auth/current', undefined, 200);
    }
    report.healthVerifiedProcessIds = pids;
    const releases = await call('GET', path);
    const baseVersion = Math.max(0, ...releases.map(r => r.version));
    report.initialBaseVersion = baseVersion;
    savedRule = await call('PUT', '/api/rules', { id, name: id, projectId: 'e2e-auto', content: 'CAP001_FROZEN_RULE_V1\n' }, 200, { 'If-None-Match': '*' });
    const publish = { name: id, expectedBaseVersion: baseVersion, ruleIds: [id], source: { path: 'e2e/api/44-team-release-multi-instance.test.mjs' } };
    const published = await Promise.all(bases.map(base => request(base, 'POST', path, publish)));
    for (const result of published) if (result.status === 201) owned.push(result.value.id);
    report.publish = published.map((r, i) => ({ processId: pids[i], status: r.status, id: r.value?.id, version: r.value?.version, code: r.value?.code })); persist();
    assert.deepEqual(published.map(r => r.status).sort(), [201, 409]);
    assert.equal(published.find(r => r.status === 409).value.code, 'intelligence_conflict');
    const first = published.find(r => r.status === 201).value;
    assert.equal(first.version, baseVersion + 1);
    const frozen = { sha256: first.sha256, payloadDigest: digest(first.payload) };
    const revoked = await call('POST', `${path}/${first.id}/revoke`, { expectedRevision: first.revision, reason: 'Disposable multi-instance restore acceptance' });
    assert.equal(revoked.state, 'revoked');
    // A changed mutable source must not change the bytes restored from history.
    savedRule = await call('PUT', '/api/rules', { id, name: id, projectId: 'e2e-auto', content: 'CAP001_MUTATED_RULE_V2\n' }, 200, { 'If-Match': JSON.stringify(savedRule.sourceRevision) });
    const restore = { expectedBaseVersion: first.version, name: `${id}-restored`, reason: 'Same-base two-process restore acceptance' };
    const restored = await Promise.all(bases.map(base => request(base, 'POST', `${path}/${first.id}/restore`, restore)));
    for (const result of restored) if (result.status === 201) owned.push(result.value.id);
    report.restore = restored.map((r, i) => ({ processId: pids[i], status: r.status, id: r.value?.id, version: r.value?.version, code: r.value?.code })); persist();
    assert.deepEqual(restored.map(r => r.status).sort(), [201, 409]);
    assert.equal(restored.find(r => r.status === 409).value.code, 'intelligence_conflict');
    const second = restored.find(r => r.status === 201).value;
    assert.equal(second.version, baseVersion + 2);
    assert.notEqual(first.id, second.id);
    assert.equal(second.sha256, frozen.sha256);
    assert.equal(digest(second.payload), frozen.payloadDigest);
    const latest = await call('GET', path);
    assert.equal(Math.max(...latest.map(r => r.version)), baseVersion + 2);
    const history = latest.filter(r => r.name === id || r.name === `${id}-restored`);
    assert.equal(history.length, 2);
    assert.deepEqual(history.map(r => r.id).sort(), [first.id, second.id].sort());
    const original = await call('GET', `${path}/${first.id}`);
    assert.equal(original.state, 'revoked');
    assert.equal(original.sha256, frozen.sha256);
    assert.equal(digest(original.payload), frozen.payloadDigest);
    assert.deepEqual(original, revoked, 'restore must not modify the revoked historical row');
    report.immutableEvidence = { releaseIds: [first.id, second.id], versions: [first.version, second.version], sha256: frozen.sha256, payloadDigest: frozen.payloadDigest, ownVersionCount: history.length, revokedHistoricalRowUnchanged: true, mutableSourceChangedBeforeRestore: true };
    report.acceptance = 'passed';
  } catch (error) {
    report.acceptance = 'failed';
    throw error;
  } finally {
    const errors = [];
    for (const releaseId of owned) {
      try { const release = await call('GET', `${path}/${releaseId}`); if (release.state === 'published') await call('POST', `${path}/${releaseId}/revoke`, { expectedRevision: release.revision, reason: 'Disposable multi-instance acceptance cleanup' }); assert.equal((await call('GET', `${path}/${releaseId}`)).state, 'revoked'); }
      catch (error) { errors.push(error.message); }
    }
    try { await call('DELETE', `/api/rules?id=${encodeURIComponent(id)}`, undefined, 204, { 'If-Match': JSON.stringify(savedRule?.sourceRevision) }); const rules = await call('GET', '/api/rules'); assert.ok(!rules.some(r => r.id === id)); report.cleanup.ruleDeleted = true; }
    catch (error) { errors.push(error.message); }
    report.cleanup.ownReleaseCount = owned.length;
    report.cleanup.allOwnReleasesRevoked = errors.length === 0;
    if (errors.length) { report.acceptance = 'failed'; report.cleanup.errors = errors; }
    persist();
    assert.deepEqual(errors, []);
  }
});
