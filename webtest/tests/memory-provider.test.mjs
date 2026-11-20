import test from 'node:test';
import assert from 'node:assert/strict';
import {loadApp,settle} from '../lib/load-app.mjs';

async function prepare(t){
  const app=await loadApp();t.after(app.close);
  for(const path of ['team-releases','memories','skill-candidates'])app.router.get(`/api/projects/project-1/${path}`,[]);
  app.evaluate(`state.currentUser={id:1,isPlatformAdmin:true};document.body.insertAdjacentHTML('beforeend',teamIntelligencePanelHTML('project-1'))`);
  await app.evaluate("loadTeamIntelligence('project-1')");await settle(3);
  return app;
}
test('private index sync is an explicit bounded batch and pending is never shown as complete',async t=>{
  const app=await prepare(t),path='/api/projects/project-1/memory-provider/sync';
  assert.equal(app.router.calls.some(call=>call.path===path),false);
  app.router.post(path,call=>{assert.deepEqual(call.body,{limit:10});return {eligible:12,ready:8,pending:4,failed:0,complete:false};});
  app.document.querySelector('[data-ti-provider-sync]').click();await settle(4);
  assert.match(app.document.querySelector('[data-ti-provider-status]').textContent,/尚未全部就绪/);
  assert.equal(app.router.calls.filter(call=>call.path===path).length,1);
  assert.equal(app.document.querySelector('[data-ti-provider-sync]').disabled,false);
});
test('provider receipt is opt-in and its immutable provenance remains visible',async t=>{
  const app=await prepare(t),path='/api/projects/project-1/memory-recall',id='recall-'+'a'.repeat(32);
  const query=app.document.querySelector('[data-ti-query]');query.value='review memory';
  app.router.post(path,call=>{
    assert.deepEqual(call.body,{query:'review memory',limit:10});return [{id:'native',title:'Native match',content:'approved text'}];
  });
  app.document.querySelector('[data-ti-recall]').click();await settle(4);
  assert.match(app.document.querySelector('[data-ti-recall-results]').textContent,/Native match/);
  app.document.querySelector('[data-ti-provider-receipt]').checked=true;
  app.document.querySelector('[data-ti-provider-receipt]').dispatchEvent(new app.window.Event('input',{bubbles:true}));
  await app.evaluate("loadTeamIntelligence('project-1')");await settle(3);
  assert.equal(app.document.querySelector('[data-ti-provider-receipt]').checked,true);
  app.router.post(path,call=>{
    assert.deepEqual(call.body,{query:'review memory',limit:10,includeReceipt:true});
    return {items:[{id:'native',title:'Ranked match',content:'approved text'}],receipt:{id,scope:{projectId:'project-1',userId:1},binding:{instanceId:'private-local',generation:'fixture-only'},hits:[{memoryId:'native',revision:2,sha256:'b'.repeat(64)}]}};
  });
  app.document.querySelector('[data-ti-recall]').click();await settle(4);
  assert.match(app.document.querySelector('[data-ti-recall-results]').textContent,new RegExp(id));
  assert.match(app.document.querySelector('[data-ti-recall-results]').textContent,/fixture-only/);
  assert.equal(app.document.querySelector('#runMemoryRecallReceipt').value,'');
  app.evaluate(`document.getElementById('runTeamRelease').innerHTML='<option value="release">Reviewed</option>';document.getElementById('runTeamMemories').innerHTML='<input type="checkbox" data-run-memory value="native" checked>';document.getElementById('runMemoryRecallReceipt').value='${id}'`);
  const input=app.evaluate('runAdvancedInput()');
  assert.deepEqual(JSON.parse(JSON.stringify(input.teamContext)),{releaseId:'release',memoryIds:['native'],memoryRecallReceiptId:id});
  app.document.querySelector('#runTeamRelease').value='';
  assert.throws(()=>app.evaluate('runAdvancedInput()'),/请选择正式团队版本/);
});
