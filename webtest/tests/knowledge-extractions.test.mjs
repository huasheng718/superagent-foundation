import test from 'node:test';
import assert from 'node:assert/strict';
import {loadApp,settle} from '../lib/load-app.mjs';

const base='/api/projects/project-1',hash='a'.repeat(64),commit='b'.repeat(40);
const source={id:'source-run',workflowId:'extract',status:'succeeded',executionContext:{projectId:'project-1',workspaceBaseSHAs:{repo:commit}},artifacts:[{id:'fixed-output',nodeId:'extract',attempt:1,name:'suggestions',path:'reports/knowledge-candidates.json',mediaType:'application/json',size:31,sha256:hash,content:'{"untrusted":"<script>"}'}]};
async function prepare(t){
  const app=await loadApp();t.after(app.close);
  for(const name of ['team-releases','memories','skill-candidates'])app.router.get(`${base}/${name}`,[]);
  app.router.get('/api/runs',[{...source,artifacts:undefined},{id:'failed',status:'failed'}]);app.router.get('/api/runs/source-run',source);
  app.evaluate(`state.currentUser={id:1,isPlatformAdmin:true};document.body.insertAdjacentHTML('beforeend',teamIntelligencePanelHTML('project-1'));afConfirm=async()=>true`);
  await app.evaluate('loadTeamIntelligence("project-1")');await settle(3);
  return app;
}
async function readSource(app){
  app.document.querySelector('[data-ke-runs]').click();await settle(3);
  const run=app.document.querySelector('[data-ke-run]');run.value=source.id;run.dispatchEvent(new app.window.Event('change'));
  app.document.querySelector('[data-ke-load]').click();await settle(4);
}
test('extraction reads explicit source and imports its exact pins without approving either candidate',async t=>{
  const app=await prepare(t);
  assert.equal(app.router.calls.some(call=>call.path==='/api/runs'&&call.search==='?projectId=project-1'),false);
  await readSource(app);
  assert.equal(app.document.querySelectorAll('[data-ke-run] option').length,2);
  assert.match(app.document.querySelector('[data-ke-review]').textContent,/<script>/);
  assert.equal(app.document.querySelector('[data-ke-review] script'),null);
  app.router.post(`${base}/knowledge-extractions`,call=>{
    assert.deepEqual(call.body,{runId:'source-run',nodeId:'extract',artifactId:'fixed-output',expectedArtifactSHA256:hash,repositoryId:'repo',expectedCommit:commit});
    return {receipt:{id:'extraction-fixed',items:[{kind:'memory',candidateId:'m'},{kind:'skill',candidateId:'s'}]},replayed:false};
  });
  app.document.querySelector('[data-ke-import]').click();await settle(6);
  assert.match(app.document.querySelector('[data-ke-result]').textContent,/extraction-fixed/);
  assert.equal(app.router.calls.some(call=>call.method==='POST'&&/decision|publish|memory-provider/.test(call.path)),false);
});
test('extraction error retains fixed source and retries only on another explicit action',async t=>{
  const app=await prepare(t);await readSource(app);
  app.router.post(`${base}/knowledge-extractions`,{status:409,body:{error:'source attempt changed'}});
  app.document.querySelector('[data-ke-import]').click();await settle(5);
  assert.match(app.document.querySelector('[data-ke-result]').textContent,/source attempt changed/);
  assert.equal(app.document.querySelector('[data-ke-run]').value,'source-run');
  assert.equal(app.router.calls.filter(call=>call.method==='POST'&&call.path.endsWith('/knowledge-extractions')).length,1);
  assert.equal(app.document.querySelector('[data-ke-import]').disabled,false);
});
test('replaced extraction panel cannot submit its pending confirmation',async t=>{
  const app=await prepare(t);await readSource(app);let confirm;
  app.window.pending=()=>new Promise(resolve=>{confirm=resolve;});app.evaluate('afConfirm=()=>window.pending()');
  app.document.querySelector('[data-ke-import]').click();await settle(2);
  await app.evaluate('loadTeamIntelligence("project-1")');confirm(true);await settle(4);
  assert.equal(app.router.calls.some(call=>call.method==='POST'&&call.path.endsWith('/knowledge-extractions')),false);
});
test('missing single repository pin cannot become importable after source loading',async t=>{
  const app=await prepare(t);app.router.get('/api/runs/source-run',{...source,executionContext:{projectId:'project-1',workspaceBaseSHAs:{}}});await readSource(app);
  assert.equal(app.document.querySelector('[data-ke-import]').disabled,true);
  assert.equal(app.router.calls.some(call=>call.method==='POST'&&call.path.endsWith('/knowledge-extractions')),false);
});
test('refresh while import is pending retains the in-flight guard in the replacement panel',async t=>{
  const app=await prepare(t);await readSource(app);let complete;
  const response={receipt:{id:'pending-extraction',items:[]},replayed:false};
  t.after(()=>complete?.(response));
  app.router.post(`${base}/knowledge-extractions`,()=>new Promise(resolve=>{complete=resolve;}));
  app.document.querySelector('[data-ke-import]').click();await settle(3);
  assert.equal(app.router.calls.filter(call=>call.method==='POST'&&call.path.endsWith('/knowledge-extractions')).length,1);
  await app.evaluate('loadTeamIntelligence("project-1")');await settle(3);
  const replacement=app.document.querySelector('[data-ke-import]');
  assert.equal(replacement.disabled,true);
  replacement.click();await settle(2);
  assert.equal(app.router.calls.filter(call=>call.method==='POST'&&call.path.endsWith('/knowledge-extractions')).length,1);
  complete(response);await settle(5);
  assert.equal(app.document.querySelector('[data-ke-import]').disabled,false);
});
test('explicit artifact selection submits the selected immutable identity instead of the first match',async t=>{
  const app=await prepare(t),other={...source.artifacts[0],id:'second-output',nodeId:'second-node',sha256:'c'.repeat(64),content:'second fixed output'};
  app.router.get('/api/runs/source-run',{...source,artifacts:[source.artifacts[0],other]});await readSource(app);
  const select=app.document.querySelector('[data-ke-artifact]');select.value=other.id;select.dispatchEvent(new app.window.Event('change'));
  assert.match(app.document.querySelector('[data-ke-artifact-detail]').textContent,/second fixed output/);
  app.router.post(`${base}/knowledge-extractions`,call=>{
    assert.deepEqual(call.body,{runId:source.id,nodeId:other.nodeId,artifactId:other.id,expectedArtifactSHA256:other.sha256,repositoryId:'repo',expectedCommit:commit});
    return {receipt:{id:'second-extraction',items:[]},replayed:false};
  });
  app.document.querySelector('[data-ke-import]').click();await settle(6);
  assert.equal(app.router.calls.filter(call=>call.method==='POST'&&call.path.endsWith('/knowledge-extractions')).length,1);
});
test('memory and skill candidate receipt controls read project-scoped evidence without approval or duplicated handlers',async t=>{
  const app=await prepare(t);
  app.router.get(`${base}/memories`,[{id:'memory-x',title:'Unreviewed memory',content:'claim',state:'candidate',revision:1,source:{extractionId:'memory-receipt'}}]);
  const candidate={id:'skill-x',projectId:'project-1',name:'Unreviewed skill',skillId:'skill-x',state:'quarantined',revision:1,sha256:hash,package:{skill:{id:'skill-x',name:'Unreviewed skill',content:'claim',files:{}},source:{extractionId:'skill-receipt'},noticePaths:[]},resources:[]};
  app.router.get(`${base}/skill-candidates`,[candidate]);app.router.get(`${base}/skill-candidates/skill-x`,candidate);
  for(const id of ['memory-receipt','skill-receipt'])app.router.get(`${base}/knowledge-extractions/${id}`,{id,projectId:'project-1',source:{artifactSHA256:hash},claim:'<script>untrusted</script>'});
  await app.evaluate('loadTeamIntelligence("project-1")');await settle(3);
  assert.equal(app.router.calls.some(call=>/knowledge-extractions\//.test(call.path)),false);
  app.document.querySelector('[data-ke-receipt="memory-receipt"]').click();await settle(3);
  const memoryResult=app.document.querySelector('[data-ke-receipt="memory-receipt"]').nextElementSibling;
  assert.match(memoryResult.textContent,/<script>untrusted<\/script>/);assert.equal(memoryResult.querySelector('script'),null);
  app.document.querySelector('[data-ti-skill-detail]').click();await settle(3);
  app.document.querySelector('[data-ke-receipt="skill-receipt"]').click();await settle(3);
  assert.match(app.document.querySelector('[data-ke-receipt="skill-receipt"]').nextElementSibling.textContent,/skill-receipt/);
  await app.evaluate('loadTeamIntelligence("project-1")');await settle(3);
  app.document.querySelector('[data-ke-receipt="memory-receipt"]').click();await settle(3);
  assert.equal(app.router.calls.filter(call=>call.path===`${base}/knowledge-extractions/memory-receipt`).length,2);
  assert.equal(app.router.calls.filter(call=>call.path===`${base}/knowledge-extractions/skill-receipt`).length,1);
  assert.equal(app.router.calls.some(call=>call.method==='POST'&&/decision|publish|memory-provider|knowledge-extractions/.test(call.path)),false);
});
