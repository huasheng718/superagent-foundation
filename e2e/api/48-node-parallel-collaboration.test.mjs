// One real Run: parallel Mac/Linux authors, then a cross-machine consumer and verifier.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { cfg, uid } from '../lib/env.mjs';
import { get, post, put, list, waitFor, track } from '../lib/api.mjs';
import { approval, createWorkflow, script, trackRun } from '../lib/fixtures.mjs';
import { runContributionHandoff } from '../lib/contribution-handoff.mjs';

const enabled = process.env.AGENTFLOW_E2E_HANDOFF === '1'
  && process.env.AGENTFLOW_E2E_LOCAL_MYSQL_CONTAINER
  && ['127.0.0.1', 'localhost'].includes(new URL(cfg.base).hostname)
  && process.platform === 'darwin';

const authorScript = kind => `set -eu
python3 - <<'PY'
import pathlib, platform, time
root = pathlib.Path.cwd()
repos = ([root] if (root / '.git').exists() else []) + [p.parent for p in root.glob('*/.git')]
assert len(repos) == 1
repo = repos[0]
kind = ${JSON.stringify(kind)}
assert platform.system() == ('Linux' if kind == 'linux-author' else 'Darwin')
time.sleep(2)
target = repo / 'parallel'
target.mkdir(parents=True, exist_ok=True)
(target / (kind + '.txt')).write_text(kind + ' accepted\\n')
(root / 'reports').mkdir(exist_ok=True)
(root / 'reports' / (kind + '.json')).write_text('{"kind":' + repr(kind) + '}')
PY`;

const consumerScript = `set -eu
python3 - <<'PY'
import pathlib, platform
root = pathlib.Path.cwd()
repos = ([root] if (root / '.git').exists() else []) + [p.parent for p in root.glob('*/.git')]
assert len(repos) == 1
repo = repos[0]
assert platform.system() == 'Darwin'
assert (repo / 'parallel' / 'mac-author.txt').read_text() == 'mac-author accepted\\n'
assert (repo / 'parallel' / 'linux-author.txt').read_text() == 'linux-author accepted\\n'
(repo / 'parallel' / 'consumer.txt').write_text('consumer saw both authors\\n')
PY`;

const verifierScript = `set -eu
python3 - <<'PY'
import pathlib, platform
root = pathlib.Path.cwd()
repos = ([root] if (root / '.git').exists() else []) + [p.parent for p in root.glob('*/.git')]
assert len(repos) == 1
repo = repos[0]
assert platform.system() == 'Linux'
assert (repo / 'parallel' / 'consumer.txt').read_text() == 'consumer saw both authors\\n'
PY`;

test('P1 one Run parallel Mac/Linux authors → cross-machine consumer → verifier', {
  skip: !enabled && 'requires explicit disposable local Mac/Docker Linux acceptance',
  timeout: 600_000,
}, async t => {
  await runContributionHandoff(t, {
    phase: 'receipts',
    entrySource: readFileSync(new URL(import.meta.url)),
    entryName: '48-node-parallel-collaboration.test.mjs',
    onWritersReady: async ({ report, repositoryId, baseSHA, persist }) => {
      const runners = Object.fromEntries(report.runners.map(r => [r.kind, r]));
      const placement = runner => ({ runnerId: runner.id, os: runner.os, arch: runner.arch });
      const nodes = [
        script('mac-author', authorScript('mac-author'), { maxDurationSec: 90 }),
        script('linux-author', authorScript('linux-author'), { maxDurationSec: 90 }),
        script('consumer', consumerScript, { maxDurationSec: 90 }),
        script('verifier', verifierScript, { maxDurationSec: 90 }),
        approval('hold', ''),
      ];
      const edges = [
        { id: 'mac-consumer', source: 'mac-author', target: 'consumer', when: 'success' },
        { id: 'linux-consumer', source: 'linux-author', target: 'consumer', when: 'success' },
        { id: 'consumer-verifier', source: 'consumer', target: 'verifier', when: 'success' },
        { id: 'verifier-hold', source: 'verifier', target: 'hold', when: 'success' },
      ];
      const workflowId = await createWorkflow('node-parallel-collaboration', nodes, { edges, sandboxProfileId: 'local' });
      const issue = await post('/api/issues', {
        projectId: cfg.projectId,
        title: `E2E parallel node collaboration ${uid('placed')}`,
        description: 'Disposable one-Run parallel Mac/Linux handoff',
        priority: 'medium', kind: 'requirement', status: 'todo', workflowId, repositoryIds: [repositoryId],
      });
      track(`issue ${issue.id}`, () => post(`/api/issues/${issue.id}/close`, {}, { expect: [200, 404, 409] }));
      const nodePlacements = {
        'mac-author': placement(runners.mac),
        'linux-author': placement(runners.linux),
        consumer: placement(runners.mac),
        verifier: placement(runners.linux),
      };
      await put('/api/runners/default', { runnerId: runners.mac.id });
      await post(`/api/issues/${issue.id}/dispatch`, {
        workflowId,
        runnerWorkspaceLayout: 'attempt',
        workspaceBaseSHAs: { [repositoryId]: baseSHA },
        nodePlacements,
      });
      const summary = await waitFor('parallel placed Run', async () => list(await get(`/api/issues/${issue.id}/runs`))[0], { timeoutMs: 30_000, intervalMs: 300 });
      const runId = summary.id || summary.runId;
      trackRun(runId);
      const waiting = await waitFor('parallel handoff gate', async () => {
        const run = await get(`/api/runs/${runId}`);
        if (['failed', 'cancelled', 'rejected'].includes(run.status)) throw new Error(`parallel Run failed: ${run.error} ${JSON.stringify(run.nodes)}`);
        return run.nodes?.hold?.status === 'waiting_approval' ? run : null;
      }, { timeoutMs: 240_000, intervalMs: 500 });
      const tasks = list(await get('/api/runners/tasks')).filter(task => task.runId === runId && ['mac-author', 'linux-author', 'consumer', 'verifier'].includes(task.nodeId));
      assert.equal(tasks.length, 4);
      const byNode = Object.fromEntries(tasks.map(task => [task.nodeId, task]));
      for (const id of Object.keys(nodePlacements)) {
        assert.equal(byNode[id].status, 'success');
        assert.equal(byNode[id].acceptedContributions?.length, 1);
        assert.equal(byNode[id].executionPlacement?.runnerId, nodePlacements[id].runnerId);
      }
      const mac = waiting.nodes['mac-author'];
      const linux = waiting.nodes['linux-author'];
      assert.ok(Date.parse(mac.startedAt) < Date.parse(linux.finishedAt));
      assert.ok(Date.parse(linux.startedAt) < Date.parse(mac.finishedAt), 'parallel author nodes must overlap');
      const consumerInputs = byNode.consumer.invocation?.contributionInputs || [];
      assert.equal(consumerInputs.length, 1);
      assert.deepEqual(new Set(consumerInputs[0].sources.map(source => source.taskId)), new Set([byNode['mac-author'].id, byNode['linux-author'].id]));
      assert.equal(byNode.verifier.invocation.contributionInputs[0].sources[0].taskId, byNode.consumer.id);
      assert.equal(byNode.verifier.invocation.contributionInputs[0].headSHA, byNode.consumer.acceptedContributions[0].resultSHA);
      await post(`/api/runs/${runId}/approve`, { nodeId: 'hold', context: 'Automated local parallel collaboration test; not human business acceptance' });
      const done = await waitFor('parallel Run cleanup', async () => {
        const run = await get(`/api/runs/${runId}`);
        if (run.status === 'failed') throw new Error(run.error || 'parallel Run failed after approval');
        return run.status === 'succeeded' && Date.parse(run.workspaceCleanedAt) > 0 ? run : null;
      }, { timeoutMs: 120_000, intervalMs: 500 });
      const integration = await post(`/api/runs/${runId}/contributions/integrate`, { repositoryId, taskIds: [byNode.consumer.id] });
      assert.equal(integration.resultSHA, byNode.consumer.acceptedContributions[0].resultSHA);
      const cleanup = list(await get('/api/runners/tasks')).filter(task => task.runId === runId && task.action === 'cleanup_run');
      assert.equal(new Set(cleanup.map(task => task.runnerId || task.leaseOwner)).size, 2);
      assert.ok(cleanup.every(task => task.status === 'success'));
      report.parallelNodeCollaboration = {
        acceptance: 'passed', runId, workflowId, physicalHosts: 1, status: done.status,
        workspaceCleanedAt: done.workspaceCleanedAt,
        overlap: { macAuthor: mac, linuxAuthor: linux },
        tasks: tasks.map(task => ({ id: task.id, nodeId: task.nodeId, status: task.status, executionPlacement: task.executionPlacement, acceptedContributions: task.acceptedContributions, contributionInputs: task.invocation?.contributionInputs })),
        integration,
        cleanup: cleanup.map(task => ({ id: task.id, status: task.status, runnerId: task.runnerId || task.leaseOwner })),
      };
      persist();
    },
  });
});
