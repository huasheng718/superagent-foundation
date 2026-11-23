// One Engine Run, two real OS Workers, accepted sequential code handoff.
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import { cfg, uid } from '../lib/env.mjs';
import { get, post, put, list, waitFor, track } from '../lib/api.mjs';
import { script, approval, createWorkflow, trackRun } from '../lib/fixtures.mjs';
import { runContributionHandoff } from '../lib/contribution-handoff.mjs';

const enabled = process.env.AGENTFLOW_E2E_HANDOFF === '1'
  && process.env.AGENTFLOW_E2E_LOCAL_MYSQL_CONTAINER === 'superagent-p0-contribution'
  && ['127.0.0.1', 'localhost'].includes(new URL(cfg.base).hostname) && process.platform === 'darwin';
const code = (kind) => `set -eu
python3 - <<'PY'
import pathlib, platform, subprocess, json
root=pathlib.Path.cwd()
repos=([root] if (root/'.git').exists() else [])+[p.parent for p in root.glob('*/.git')]
assert len(repos)==1
repo=repos[0]
kind=${JSON.stringify(kind)}
assert platform.system()==('Linux' if kind=='consumer' else 'Darwin')
a=repo/'collaboration'/'mac.txt'
b=repo/'collaboration'/'linux.txt'
if kind=='author':
    assert not a.exists() and not b.exists()
    a.parent.mkdir(parents=True,exist_ok=True)
    a.write_text('accepted mac contribution\\n')
else:
    assert a.read_text()=='accepted mac contribution\\n'
    if kind=='consumer':
        assert not b.exists()
        b.write_text('linux consumed accepted mac commit\\n')
    else:
        assert b.read_text()=='linux consumed accepted mac commit\\n'
report={'kind':kind,'os':platform.system(),'head':subprocess.check_output(['git','-C',str(repo),'rev-parse','HEAD'],text=True).strip(),'a':a.read_text(),'b':b.read_text() if b.exists() else None}
(root/'reports').mkdir(exist_ok=True)
(root/'reports'/('collaboration-'+kind+'.json')).write_text(json.dumps(report))
print(json.dumps(report))
PY`;

test('P1 one Run Mac author → Linux consumer → Mac verifier', {
  skip: !enabled && 'requires explicit disposable local Mac/Docker Linux acceptance', timeout: 600_000,
}, async t => {
  await runContributionHandoff(t, { phase: 'receipts', entrySource: readFileSync(new URL(import.meta.url)), entryName: '42-node-collaboration.test.mjs',
    onWritersReady: async ({ report, repositoryId, baseSHA, persist }) => {
      const runners=Object.fromEntries(report.runners.map(r=>[r.kind,r]));
      await put('/api/runners/default',{runnerId:runners.mac.id});
      const nodes=['author','consumer','verifier'].map(kind=>script(kind,code(kind),{
        maxDurationSec:90,artifactOutputs:[{name:kind,path:`reports/collaboration-${kind}.json`,mediaType:'application/json',required:true}],
      }));
      nodes.push(approval('hold',''));
      const workflowId=await createWorkflow('node-collaboration',nodes,{sandboxProfileId:'local'});
      const issue=await post('/api/issues',{projectId:cfg.projectId,title:`E2E node collaboration ${uid('placed')}`,description:'Disposable single Run cross-OS handoff',priority:'medium',kind:'requirement',status:'todo',workflowId,repositoryIds:[repositoryId]});
      track(`issue ${issue.id}`,()=>post(`/api/issues/${issue.id}/close`,{},{expect:[200,404,409]}));
      const placement=(runner)=>({runnerId:runner.id,os:runner.os,arch:runner.arch});
      const input={workflowId,runnerWorkspaceLayout:'attempt',workspaceBaseSHAs:{[repositoryId]:baseSHA},nodePlacements:{author:placement(runners.mac),consumer:placement(runners.linux),verifier:placement(runners.mac)}};
      await post(`/api/issues/${issue.id}/dispatch`,input);
      const summary=await waitFor('placed run',async()=>list(await get(`/api/issues/${issue.id}/runs`))[0],{timeoutMs:30_000,intervalMs:300});
      const runId=summary.id||summary.runId;trackRun(runId);
      report.boundaries.nodeCollaboration='one existing Engine Run; sequential Mac/Linux/Mac nodes; separate legacy two-Run receipt regression also exercised';
      report.nodeCollaboration={runId,workflowId,acceptance:'running',physicalHosts:1};persist();
      const run=await waitFor('placed sequence',async()=>{
        const value=await get(`/api/runs/${runId}`);
        if(['failed','cancelled','rejected'].includes(value.status)) throw new Error(`placed run failed: ${value.error} ${JSON.stringify(value.nodes)}`);
        return value.nodes?.hold?.status==='waiting_approval'?value:null;
      },{timeoutMs:180_000,intervalMs:500});
      const tasks=list(await get('/api/runners/tasks')).filter(task=>task.runId===runId&&['author','consumer','verifier'].includes(task.nodeId));
      assert.equal(tasks.length,3);
      const byNode=Object.fromEntries(tasks.map(task=>[task.nodeId,task]));
      for(const kind of ['author','consumer','verifier']){
        const task=byNode[kind];assert.equal(task.status,'success');assert.equal(task.acceptedContributions?.length,1);
        assert.equal(task.runnerId||task.leaseOwner,kind==='consumer'?runners.linux.id:runners.mac.id);
        assert.equal(task.executionPlacement?.runnerId,kind==='consumer'?runners.linux.id:runners.mac.id);
      }
      assert.equal(byNode.consumer.invocation.contributionInputs[0].headSHA,byNode.author.acceptedContributions[0].resultSHA);
      assert.equal(byNode.verifier.invocation.contributionInputs[0].headSHA,byNode.consumer.acceptedContributions[0].resultSHA);
      assert.equal(byNode.verifier.acceptedContributions[0].resultSHA,byNode.consumer.acceptedContributions[0].resultSHA,'verification must not alter source tree');
      await post(`/api/runs/${runId}/approve`,{nodeId:'hold',context:'Automated local collaboration test; not human business acceptance'});
      const done=await waitFor('placed run cleanup',async()=>{
        const value=await get(`/api/runs/${runId}`);
        if(value.status==='failed')throw new Error(value.error);
        return value.status==='succeeded'&&Date.parse(value.workspaceCleanedAt)>0?value:null;
      },{timeoutMs:90_000,intervalMs:500});
      const integration=await post(`/api/runs/${runId}/contributions/integrate`,{repositoryId,taskIds:[byNode.author.id,byNode.consumer.id]});
      assert.equal(integration.resultSHA,byNode.consumer.acceptedContributions[0].resultSHA);
      const maintenance=list(await get('/api/runners/tasks')).filter(task=>task.runId===runId&&task.action==='cleanup_run');
      assert.equal(maintenance.length,2,'one cleanup acknowledgement per actual Worker');
      assert.equal(new Set(maintenance.map(task=>task.runnerId||task.leaseOwner)).size,2,'both actual Workers clean their own workspace');
      assert.ok(maintenance.every(task=>task.status==='success'));
      // Retain only non-secret, public receipt/placement facts.
      report.nodeCollaboration={runId,workflowId,acceptance:'passed',physicalHosts:1,status:done.status,workspaceCleanedAt:done.workspaceCleanedAt,integration,tasks:tasks.map(task=>({id:task.id,nodeId:task.nodeId,attempt:task.attempt,status:task.status,executionPlacement:task.executionPlacement,acceptedContributions:task.acceptedContributions})),cleanup:maintenance.map(task=>({id:task.id,status:task.status,runnerId:task.runnerId||task.leaseOwner}))};persist();
    },
  });
});
