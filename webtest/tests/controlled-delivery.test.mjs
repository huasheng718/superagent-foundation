import test from 'node:test';
import assert from 'node:assert/strict';
import { loadApp, settle } from '../lib/load-app.mjs';

const sha='a'.repeat(40),next='b'.repeat(40),hash='c'.repeat(64);
function runFixture(app,id='run-controlled') {
  return {id,status:'succeeded',createdAt:'2026-11-17T10:00:00Z',updatedAt:'2026-11-17T10:01:00Z',
    executionContext:{projectId:'project-1',deliveryPolicy:'controlled_pr'},
    repositories:[{repositoryId:'repo-1',baseSHA:sha}],
    workflowSnapshot:app.evaluate('structuredClone(state.data.workflows[0])'),nodes:{},artifacts:[]};
}
function prepare(app,run){
  app.evaluate(`state.currentUser={id:1,isPlatformAdmin:true};state.selectedRun=${JSON.stringify(run.id)};state.activeRun=${JSON.stringify(run)};state.data.runs=[state.activeRun];state.runDetailTab='delivery';renderRunDetail(state.activeRun)`);
}
const task={taskId:'author-task',runId:'source-run',nodeId:'author',status:'success',attempt:1,contributions:[{taskId:'author-task',repositoryId:'repo-1',baseSHA:sha,resultSHA:next,attempt:1,acceptedAt:'2026-11-17T10:00:00Z'}]};
const integration={id:'integration-1',repositoryId:'repo-1',runId:'run-controlled',resultSHA:next,contextSHA256:hash,state:'prepared'};

test('controlled delivery exposes only accepted successful contributions and sends selected integration input',async t=>{
  const app=await loadApp();t.after(app.close);const run=runFixture(app);
  let data={tasks:[task,{...task,taskId:'failed-task',status:'failed'},{...task,taskId:'unaccepted',contributions:[{...task.contributions[0],acceptedAt:'0001-01-01T00:00:00Z'}]}],integrations:[],publications:[]};
  app.router.get('/api/runs/run-controlled/contributions',()=>data);
  app.router.post('/api/runs/run-controlled/contributions/integrate',call=>{assert.deepEqual(call.body,{repositoryId:'repo-1',taskIds:['author-task']});data={...data,integrations:[integration]};return integration;});
  prepare(app,run);await settle(8);
  assert.equal(app.document.querySelectorAll('[data-cd-task]').length,1);
  const box=app.document.querySelector('[data-cd-task]');box.click();
  assert.equal(app.document.querySelector('[data-cd-integrate]').disabled,false);
  app.document.querySelector('[data-cd-integrate]').click();await settle(8);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/integrate')).length,1);
  assert.match(app.document.querySelector('[data-run-pane="delivery"]').textContent,/bbbbbbbbbbbb/);
  assert.equal(app.document.querySelector('[data-cd-publish]').disabled,true);
});

test('publication failure remains an inline error and never fabricates a PR receipt',async t=>{
  const app=await loadApp();t.after(app.close);const run=runFixture(app);
  app.router.get('/api/runs/run-controlled/contributions',{tasks:[task],integrations:[integration],publications:[{...integration,state:'ready',gates:[]}]});
  app.router.post('/api/runs/run-controlled/contributions/integration-1/publish',{error:'gate evidence revoked'},409);
  prepare(app,run);await settle(8);app.evaluate('afConfirm=async()=>true');
  assert.equal(app.document.querySelector('[data-cd-publish]').disabled,false);
  app.document.querySelector('[data-cd-publish]').click();await settle(8);
  assert.match(app.document.querySelector('[data-run-pane="delivery"] [role="alert"]').textContent,/gate evidence revoked/);
  assert.equal(app.document.querySelectorAll('[data-run-pane="delivery"] a').length,0);
});

test('PR URLs require HTTPS and no userinfo; unsafe links are not rendered',async t=>{
  const app=await loadApp();t.after(app.close);
  app.router.get('/api/runs/run-controlled/contributions',{tasks:[],integrations:[integration],publications:[{...integration,state:'pr_ready',prURL:'javascript:alert(1)'}]});
  prepare(app,runFixture(app));await settle(8);
  assert.equal(app.document.querySelectorAll('[data-run-pane="delivery"] a').length,0);
  assert.equal(app.evaluate("deliverySafePR('https://user:pass@example.com/pr/1')"),'');
  assert.equal(app.evaluate("deliverySafePR('https://github.com/example/repo/pull/1')"),'https://github.com/example/repo/pull/1');
});

test('gate registration uses actual chosen validation node and immutable report',async t=>{
  const app=await loadApp();t.after(app.close);const run=runFixture(app);
  app.router.get('/api/runs/run-controlled/contributions',{tasks:[],integrations:[integration],publications:[{...integration,state:'gating',gates:[]}]});
  const gate={id:'gate-run',status:'succeeded',workflowSnapshot:{nodes:[{id:'fast-node',name:'Fast test',type:'script'}]},nodes:{'fast-node':{status:'succeeded'}},artifacts:[{id:'report-1',nodeId:'fast-node',name:'Immutable report',attempt:1}]};
  app.router.get('/api/runs/gate-run',gate);
  app.router.post('/api/runs/run-controlled/contributions/integration-1/gates',call=>{assert.deepEqual(call.body,{gateRunId:'gate-run',nodeId:'fast-node',kind:'fast',artifactId:'report-1'});return {};});
  prepare(app,run);app.evaluate(`state.data.runs.push(${JSON.stringify(gate)})`);await settle(8);app.evaluate('renderControlledDelivery(state.activeRun)');
  const select=app.document.querySelector('[data-cd-gate-run]');select.value='gate-run';select.dispatchEvent(new app.window.Event('change',{bubbles:true}));await settle(8);
  assert.equal(app.document.querySelector('[data-cd-record-gate]').disabled,false);
  app.document.querySelector('[data-cd-record-gate]').click();await settle(8);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/gates')).length,1);
});

test('non-admin UI exposes no gate registration and cannot integrate',async t=>{
  const app=await loadApp();t.after(app.close);const run=runFixture(app);
  app.router.get('/api/runs/run-controlled/contributions',{tasks:[task],integrations:[integration],publications:[]});
  prepare(app,run);await settle(8);app.evaluate('state.currentUser.isPlatformAdmin=false;renderControlledDelivery(state.activeRun)');
  assert.equal(app.document.querySelector('[data-cd-task]').disabled,true);
  assert.equal(app.document.querySelector('[data-cd-integrate]').disabled,true);
  assert.equal(app.document.querySelector('[data-cd-record-gate]'),null);
});

test('budget form uses exact USD micros and rejects missing caps or unsafe integers',async t=>{
  const app=await loadApp();t.after(app.close);
  assert.equal(app.evaluate("runUSDToMicros('0.000001')"),1);
  assert.equal(app.evaluate("runUSDToMicros('2.50')"),2500000);
  assert.throws(()=>app.evaluate("runUSDToMicros('1e3')"));
  assert.throws(()=>app.evaluate("runUSDToMicros('1.0000001')"));
  app.document.getElementById('runBudgetEnabled').checked=true;
  assert.throws(()=>app.evaluate('runAdvancedInput()'),/最多调用次数/);
  app.document.getElementById('runBudgetInvocations').value='3';
  app.document.getElementById('runBudgetNoProgress').value='2';
  app.document.getElementById('runBudgetCost').value='2.50';
  assert.throws(()=>app.evaluate('runAdvancedInput()'),/价格版本/);
  app.document.getElementById('runBudgetPrice').value='reviewed-usd-v1';
  const actual=JSON.parse(app.evaluate('JSON.stringify(runAdvancedInput())'));
  assert.deepEqual(actual.executionBudget,{mode:'metered',currency:'USD',maxInvocations:3,maxNoProgress:2,maxCostMicrosUSD:2500000,priceVersion:'reviewed-usd-v1'});
});

test('memory decisions use current revision and explicit human reason',async t=>{
  const app=await loadApp();t.after(app.close);
  const memory={id:'memory-1',title:'Verified rule',content:'Check lease generation',state:'candidate',revision:4,source:{path:'docs/rules.md'}};
  app.router.get('/api/projects/project-1/team-releases',[]);
  app.router.get('/api/projects/project-1/memories',[memory]);
  app.router.post('/api/projects/project-1/memories/memory-1/decision',call=>{assert.deepEqual(call.body,{decision:'approved',expectedRevision:4,reason:'Reviewed test evidence'});return {...memory,state:'approved',revision:5};});
  app.evaluate(`state.currentUser={id:1,isPlatformAdmin:true};document.body.insertAdjacentHTML('beforeend',teamIntelligencePanelHTML('project-1'));afPrompt=async()=> 'Reviewed test evidence'`);
  await app.evaluate("loadTeamIntelligence('project-1')");await settle(3);
  app.document.querySelector('[data-ti-decision="approved"]').click();await settle(8);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/decision')).length,1);
});

test('isolated manual runs pin only explicitly selected repositories and keep approval nodes human',async t=>{
  const app=await loadApp();t.after(app.close);
  app.evaluate(`state.data.repositories.push({...state.data.repositories[0],id:'repo-2',name:'Second repo'});state.data.runners=[{id:'runner-1',name:'Linux',status:'online'}];document.getElementById('runWorkflowChoice').innerHTML='<option value="flow-1">Delivery</option>';prepareRunAdvancedControls()`);
  assert.equal(app.document.querySelectorAll('[data-run-repo-choice]:checked').length,0);
  assert.equal(app.document.querySelectorAll('[data-run-node-runner]').length,1);
  const choice=app.document.querySelector('[data-run-repo-choice][value="repo-2"]');choice.click();
  const pin=app.document.querySelector('[data-run-repo-pin="repo-2"]');assert.equal(pin.disabled,false);pin.value=sha;
  app.document.querySelector('[data-run-node-runner]').value='runner-1';
  app.document.getElementById('runIsolatedEnabled').checked=true;
  const input=JSON.parse(app.evaluate('JSON.stringify(runAdvancedInput())'));
  assert.deepEqual(input.repositoryIds,['repo-2']);assert.deepEqual(input.workspaceBaseSHAs,{'repo-2':sha});
  assert.deepEqual(input.nodePlacements,{'node-1':{runnerId:'runner-1'}});
  app.evaluate('state.data.workflows[0].generic=true;prepareRunAdvancedControls()');
  assert.equal(app.document.querySelector('[data-run-repo-choice]:checked').value,'repo-1');
});

test('restore publishes historical release as a new version and preserves fixed context display',async t=>{
  const app=await loadApp();t.after(app.close);
  const release={id:'release-1',name:'Reviewed config',state:'revoked',version:1,revision:2};
  app.router.get('/api/projects/project-1/team-releases',[release]);app.router.get('/api/projects/project-1/memories',[]);
  app.router.post('/api/projects/project-1/team-releases/release-1/restore',call=>{assert.deepEqual(call.body,{name:'Reviewed config',reason:'Restore proven baseline',expectedBaseVersion:1});return {...release,id:'release-2',state:'published',version:2};});
  app.evaluate(`state.currentUser={id:1,isPlatformAdmin:true};document.body.insertAdjacentHTML('beforeend',teamIntelligencePanelHTML('project-1'));afPrompt=async()=> 'Restore proven baseline'`);
  await app.evaluate("loadTeamIntelligence('project-1')");app.document.querySelector('[data-ti-restore]').click();await settle(8);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/restore')).length,1);
  const html=app.evaluate(`intelligenceSummaryHTML({intelligence:{releaseId:'release-1',releaseSha256:'fixed-sha',sha256:'context-sha',memories:[{id:'memory-1'}]}})`);
  assert.match(html,/release-1/);assert.match(html,/context-sha/);assert.match(html,/1 条/);
});

test('run context selection excludes revoked, expired and invalid-expiry memories',async t=>{
  const app=await loadApp();t.after(app.close);
  app.router.get('/api/projects/project-1/team-releases',[]);
  app.router.get('/api/projects/project-1/memories',[
    {id:'valid',title:'Approved valid',state:'approved'},
    {id:'expired',title:'Expired',state:'approved',expiresAt:'2000-01-01T00:00:00Z'},
    {id:'invalid',title:'Invalid expiry',state:'approved',expiresAt:'broken'},
    {id:'revoked',title:'Revoked',state:'revoked'}
  ]);
  await app.evaluate('loadRunTeamSelections()');
  assert.deepEqual([...app.document.querySelectorAll('[data-run-memory]')].map(input=>input.value),['valid']);
});


test('release conflicts preserve drafts and require an explicit refresh before rebasing',async t=>{
  const app=await loadApp();t.after(app.close);
  let version=2;
  app.router.get('/api/projects/project-1/team-releases',()=>[{id:'release-'+version,name:'Current',state:'published',version}]);
  app.router.get('/api/projects/project-1/memories',[]);
  app.router.post('/api/projects/project-1/team-releases',call=>{
    assert.equal(call.body.expectedBaseVersion,version===3?3:2);
    assert.equal(call.body.name,'My reviewed draft');
    return {status:409,body:{code:'intelligence_conflict',error:'intelligence revision or lifecycle conflict'}};
  });
  app.evaluate(`state.currentUser={id:1,isPlatformAdmin:true};document.body.insertAdjacentHTML('beforeend',teamIntelligencePanelHTML('project-1'));renderTeamIntelligence('project-1');afConfirm=async()=>true`);
  assert.equal(app.document.querySelector('[data-ti-publish]').disabled,true);
  await app.evaluate("loadTeamIntelligence('project-1')");
  const name=app.document.querySelector('[data-ti-release-name]');name.value='My reviewed draft';name.dispatchEvent(new app.window.Event('input',{bubbles:true}));
  app.document.querySelector('[data-ti-publish]').click();await settle(8);
  assert.match(app.document.querySelector('[data-ti-project]').textContent,/手动刷新后复核并重试/);
  assert.equal(app.document.querySelector('[data-ti-release-name]').value,'My reviewed draft');
  assert.equal(app.router.calls.filter(call=>call.method==='POST'&&call.path.endsWith('/team-releases')).length,1);
  version=3;
  app.document.querySelector('[data-ti-refresh]').click();await settle(8);
  assert.equal(app.document.querySelector('[data-ti-release-name]').value,'My reviewed draft');
  app.document.querySelector('[data-ti-publish]').click();await settle(8);
  assert.equal(app.router.calls.filter(call=>call.method==='POST'&&call.path.endsWith('/team-releases')).length,2);
});


test('default upgrade comparison shows three-way paths without changing team overrides',async t=>{
  const app=await loadApp();t.after(app.close);
  app.router.get('/api/projects/project-1/team-releases',[]);app.router.get('/api/projects/project-1/memories',[]);
  app.router.get('/api/projects/project-1/default-upgrades',[{entityId:'role-1',kind:'agent',tracked:true,defaultAvailable:true,defaultChangedPaths:['/prompt'],overridePaths:['/prompt','/name'],conflictingPaths:['/prompt'],baseDefault:{prompt:'Base'},currentDefault:{prompt:'Current'},teamOverride:{prompt:'Reviewed override'}},{entityId:'legacy-skill',kind:'skill',tracked:false}]);
  app.evaluate(`state.currentUser={id:1,isPlatformAdmin:true};document.body.insertAdjacentHTML('beforeend',teamIntelligencePanelHTML('project-1'))`);
  await app.evaluate("loadTeamIntelligence('project-1')");
  app.document.querySelector('[data-ti-upgrades]').click();await settle(8);
  const text=app.document.querySelector('[data-ti-upgrade-results]').textContent;
  assert.match(text,/冲突字段：\/prompt/);assert.match(text,/Reviewed override/);assert.match(text,/未记录历史基线/);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/default-upgrades')).length,1);
  assert.equal(app.router.calls.filter(call=>call.method!=='GET').length,0);
});


test('formal release asset choices match project/shared and project-only MCP scope',async t=>{
  const app=await loadApp();t.after(app.close);
  app.evaluate(`state.currentUser={id:1,isPlatformAdmin:true};state.data.agents=[
    {id:'owned',name:'Owned',projectId:'project-1'},
    {id:'shared',name:'Shared',projectId:'other',shared:true},
    {id:'foreign',name:'Foreign',projectId:'other'},
    {id:'generic-unadopted',name:'Generic',generic:true},
    {id:'legacy-unscoped',name:'Legacy'}
  ];state.data.mcpServers=[
    {id:'project-mcp',name:'Project',scope:'project',projectId:'project-1'},
    {id:'personal-mcp',name:'Personal',scope:'personal',projectId:'project-1',ownerUserId:1},
    {id:'foreign-mcp',name:'Foreign',scope:'project',projectId:'other'},
    {id:'legacy-mcp',name:'Legacy'},
    {id:'bad-owner',name:'Bad owner',scope:'project',projectId:'project-1',ownerUserId:1}
  ];document.body.insertAdjacentHTML('beforeend',teamIntelligencePanelHTML('project-1'));renderTeamIntelligence('project-1')`);
  assert.deepEqual([...app.document.querySelectorAll('[data-ti-asset="agents"]')].map(field=>field.value),['owned','shared']);
  assert.deepEqual([...app.document.querySelectorAll('[data-ti-asset="mcpServers"]')].map(field=>field.value),['project-mcp']);
});


test('uncertain PR publication offers discovery only after lease expiry and retains unresolved state',async t=>{
  const app=await loadApp();t.after(app.close);const run=runFixture(app);
  const pending={...integration,state:'publishing',prCreateAttempted:true,phaseVersion:1,leaseUntil:'2000-01-01T00:00:00Z'};
  app.router.get('/api/runs/run-controlled/contributions',{tasks:[task],integrations:[integration],publications:[pending]});
  app.router.post('/api/runs/run-controlled/contributions/integration-1/publish',{error:'Existing PR not yet visible; manual reconciliation required'},409);
  app.evaluate('afConfirm=async()=>true');prepare(app,run);await settle(8);
  assert.match(app.document.querySelector('[data-run-pane="delivery"]').textContent,/结果待核对/);
  const button=app.document.querySelector('[data-cd-publish]');assert.equal(button.textContent,'核对已有 PR');assert.equal(button.disabled,false);
  button.click();await settle(8);
  assert.match(app.document.querySelector('[data-run-pane="delivery"]').textContent,/not yet visible/);
  assert.equal(app.document.querySelectorAll('[data-run-pane="delivery"] a').length,0);
  pending.leaseUntil='2999-01-01T00:00:00Z';
  app.router.get('/api/runs/run-controlled/contributions',{tasks:[task],integrations:[integration],publications:[pending]});
  await app.evaluate('loadControlledDelivery(state.activeRun.id)');await settle(8);
  assert.equal(app.document.querySelector('[data-cd-publish]').disabled,true);
});
