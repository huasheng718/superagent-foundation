import test from 'node:test';
import assert from 'node:assert/strict';
import { loadApp, settle } from '../lib/load-app.mjs';

async function prepare(t){
  const app=await loadApp();t.after(app.close);
  app.router.get('/api/projects/project-1/team-releases',[{id:'old',version:3}]);
  app.router.get('/api/projects/project-1/memories',[]);
  app.router.get('/api/projects/project-1/skill-candidates',[]);
  app.evaluate(`state.currentUser={id:1,isPlatformAdmin:true};state.data.repositories=[{id:'own',name:'Team config',projectId:'project-1'},{id:'foreign',name:'Private',projectId:'project-2'}];document.body.insertAdjacentHTML('beforeend',teamIntelligencePanelHTML('project-1'))`);
  await app.evaluate("loadTeamIntelligence('project-1')");await settle(3);
  const fill=(key,value)=>{const field=app.document.querySelector(`[data-tg-field="${key}"]`);field.value=value;field.dispatchEvent(new app.window.Event('input',{bubbles:true}));};
  fill('repositoryId','own');fill('commit','a'.repeat(40));fill('name','Reviewed config');
  app.evaluate('afConfirm=async()=>true');
  return {app,fill};
}

test('release detail exports the selected binary resource with the reviewed release digest',async t=>{
  const {app}=await prepare(t);
  const release={id:'old',state:'published',sha256:'c'.repeat(64),payload:{gitImport:{files:[{path:'teamai.yaml',mode:'100644'},{path:'LICENSE',mode:'100644'},{path:'skills/read/data.bin',mode:'100755'},{path:'rules/review.md',mode:'100644'}]}}};
  app.router.get('/api/projects/project-1/team-releases/old',release);
  app.router.post('/api/projects/project-1/team-releases/old/git-export-preview',call=>{
    assert.deepEqual(call.body,{expectedReleaseSha256:release.sha256,changes:[{path:'skills/read/data.bin',content:'AP+A',executable:true}]});
    return {baseCommit:'a'.repeat(40),patch:'binary patch',proposed:{sha256:'d'.repeat(64)}};
  });
  app.document.querySelector('[data-ti-release-detail]').click();await settle(5);
  assert.equal(app.document.querySelectorAll('[data-tg-export-path] option').length,2);
  assert.equal(app.document.querySelector('[data-tg-export-executable]').checked,true);
  const upload=app.document.querySelector('[data-tg-export-file]');
  Object.defineProperty(upload,'files',{value:[{size:3,arrayBuffer:async()=>Uint8Array.from([0,255,128]).buffer}]});
  app.document.querySelector('[data-tg-export-preview]').click();await settle(6);
  assert.match(app.document.querySelector('[data-tg-export-result]').textContent,/binary patch/);
  assert.ok(app.document.querySelector('[data-tg-download]'));
  app.document.querySelector('[data-tg-export-executable]').dispatchEvent(new app.window.Event('change'));
  assert.equal(app.document.querySelector('[data-tg-download]'),null);
  app.document.querySelector('[data-tg-export-preview]').click();await settle(6);
  upload.dispatchEvent(new app.window.Event('change'));
  assert.equal(app.document.querySelector('[data-tg-download]'),null);
  app.document.querySelector('[data-tg-export-preview]').click();await settle(6);
  const path=app.document.querySelector('[data-tg-export-path]');path.value='rules/review.md';path.dispatchEvent(new app.window.Event('change'));
  assert.equal(app.document.querySelector('[data-tg-download]'),null);
  assert.equal(app.document.querySelector('[data-tg-export-executable]').checked,false);
});

test('release detail hides export for revoked releases and non-manager project readers',async t=>{
  const {app}=await prepare(t);
  const release={id:'old',state:'revoked',sha256:'c'.repeat(64),payload:{gitImport:{files:[{path:'skills/read/data.bin',mode:'100644'}]}}};
  app.router.get('/api/projects/project-1/team-releases/old',release);
  app.document.querySelector('[data-ti-release-detail]').click();await settle(5);
  assert.equal(app.document.querySelector('[data-tg-export]'),null);
  app.evaluate('state.currentUser={id:2,isPlatformAdmin:false};state.data.projects[0].ownerId=1');
  app.router.get('/api/projects/project-1/team-releases/old',{...release,state:'published'});
  app.document.querySelector('[data-ti-release-detail]').click();await settle(5);
  assert.equal(app.document.querySelector('[data-tg-export]'),null);
  assert.equal(app.router.calls.some(call=>call.path.endsWith('/git-export-preview')),false);
});

test('Git import retains reviewed source and stale publication base without retry',async t=>{
  const {app}=await prepare(t);
  assert.equal(app.document.querySelector('[data-tg-field="repositoryId"] option[value="foreign"]'),null);
  const preview={format:'tencent-teamai.v1',sha256:'b'.repeat(64),payload:{source:{commit:'a'.repeat(40)}},notImported:['namespace distribution']};
  let observed;
  app.router.post('/api/projects/project-1/team-imports/preview',call=>{observed=call.body;return preview;});
  app.router.post('/api/projects/project-1/team-imports/publish',call=>{
    assert.deepEqual(call.body,{...observed,name:'Reviewed config',expectedBaseVersion:3,expectedPreviewSha256:preview.sha256,approve:true});
    return {status:409,body:{error:'source base changed',code:'intelligence_conflict'}};
  });
  app.document.querySelector('[data-tg-preview]').click();await settle(6);
  assert.equal(app.document.querySelector('[data-tg-publish]').disabled,false);
  app.evaluate("intelligenceView('project-1').releases.push({version:4})");
  app.document.querySelector('[data-tg-publish]').click();await settle(6);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/team-imports/publish')).length,1);
  assert.match(app.document.querySelector('[data-tg-result]').textContent,/source base changed/);
  assert.equal(app.document.querySelector('[data-tg-field="name"]').value,'Reviewed config');
});

test('editing a reviewed Git selection invalidates approval and unsupported data is visible',async t=>{
  const {app,fill}=await prepare(t);
  app.router.post('/api/projects/project-1/team-imports/preview',{sha256:'b'.repeat(64),payload:{},notImported:['roles are not worker permissions']});
  app.document.querySelector('[data-tg-preview]').click();await settle(6);
  assert.match(app.document.querySelector('[data-tg-result]').textContent,/roles are not worker permissions/);
  fill('root','.teamai');
  assert.equal(app.document.querySelector('[data-tg-publish]').disabled,true);
  app.router.post('/api/projects/project-1/team-imports/preview',{error:'agents/reviewer.yaml: tools are unsupported'},422);
  app.document.querySelector('[data-tg-preview]').click();await settle(6);
  assert.match(app.document.querySelector('[data-tg-result]').textContent,/tools are unsupported/);
  assert.equal(app.document.querySelector('[data-tg-publish]').disabled,true);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/team-imports/publish')).length,0);
});

test('refresh during Git preview retains draft and updates the live review controls',async t=>{
  const {app}=await prepare(t);
  let complete;
  app.router.post('/api/projects/project-1/team-imports/preview',()=>new Promise(resolve=>{complete=resolve;}));
  app.document.querySelector('[data-tg-preview]').click();await settle(3);
  assert.equal(app.document.querySelector('[data-tg-preview]').disabled,true);
  await app.evaluate("loadTeamIntelligence('project-1')");
  assert.equal(app.document.querySelector('[data-tg-field="name"]').value,'Reviewed config');
  complete({sha256:'c'.repeat(64),payload:{original:'review me'}});await settle(6);
  assert.match(app.document.querySelector('[data-tg-result]').textContent,/review me/);
  assert.equal(app.document.querySelector('[data-tg-preview]').disabled,false);
  assert.equal(app.document.querySelector('[data-tg-publish]').disabled,false);
  assert.equal(app.document.querySelector('[data-tg-field="name"]').disabled,false);
});

test('Git publication cannot double submit while confirmation is pending',async t=>{
  const {app}=await prepare(t);
  app.router.post('/api/projects/project-1/team-imports/preview',{sha256:'d'.repeat(64),payload:{}});
  app.document.querySelector('[data-tg-preview]').click();await settle(5);
  let confirm;
  app.window.pendingGitConfirmation=new Promise(resolve=>{confirm=resolve;});
  app.evaluate('afConfirm=()=>pendingGitConfirmation');
  app.router.post('/api/projects/project-1/team-imports/publish',call=>{
    assert.equal(call.body.expectedPreviewSha256,'d'.repeat(64));
    return {id:'published',version:4};
  });
  const publish=app.document.querySelector('[data-tg-publish]');
  publish.click();publish.click();await settle(3);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/team-imports/publish')).length,0);
  confirm(true);await settle(8);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/team-imports/publish')).length,1);
});

async function prepareUpdate(t){
  const {app}=await prepare(t);
  const release={id:'old',name:'Reviewed config',state:'published',sha256:'c'.repeat(64),payload:{gitImport:{repositoryId:'own',commit:'a'.repeat(40),root:'.',selection:{rules:['rules/review.md']},files:[]}}};
  const update={status:'updated',baseReleaseId:release.id,baseReleaseSha256:release.sha256,expectedBaseVersion:3,ref:'refs/heads/main',commit:'b'.repeat(40),preview:{sha256:'d'.repeat(64),payload:{gitImport:{...release.payload.gitImport,commit:'b'.repeat(40),ref:'refs/heads/main'}},notImported:['targets do not assign workers']},changes:[{path:'rules/review.md',kind:'changed'}]};
  app.router.get('/api/projects/project-1/team-releases/old',release);
  app.document.querySelector('[data-ti-release-detail]').click();await settle(5);
  return {app,release,update};
}

test('branch update approval freezes source and version, retains rejected review and requires a fresh check',async t=>{
  const {app,release,update}=await prepareUpdate(t);
  app.router.post('/api/projects/project-1/team-releases/old/git-update-preview',call=>{
    assert.deepEqual(call.body,{expectedReleaseSha256:release.sha256});return update;
  });
  app.router.post('/api/projects/project-1/team-imports/publish',call=>{
    assert.deepEqual(call.body,{repositoryId:'own',commit:update.commit,root:'.',selection:{rules:['rules/review.md']},name:'Reviewed config',expectedBaseVersion:3,expectedPreviewSha256:update.preview.sha256,approve:true,updateFrom:{releaseId:release.id,releaseSha256:release.sha256,ref:update.ref}});
    return {status:409,body:{error:'registered ref changed',code:'team_import_ref_conflict'}};
  });
  app.document.querySelector('[data-tg-update-check]').click();await settle(5);
  const result=app.document.querySelector('[data-tg-update-result]');
  assert.match(result.textContent,/targets do not assign workers/);assert.match(result.textContent,/refs\/heads\/main/);
  app.evaluate("intelligenceView('project-1').releases.push({version:7})");
  app.document.querySelector('[data-tg-update-publish]').click();await settle(6);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/team-imports/publish')).length,1);
  assert.match(result.textContent,/registered ref changed/);assert.match(result.textContent,/重新检查/);
  assert.equal(app.document.querySelector('[data-tg-update-publish]').disabled,true);
  assert.equal(app.document.querySelector('[data-tg-update-name]').value,'Reviewed config');
  assert.match(result.textContent,/targets do not assign workers/);
  app.document.querySelector('[data-tg-update-publish]').click();await settle(2);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/team-imports/publish')).length,1);
});

test('unchanged branch offers no publication and revoked/non-manager releases hide checks',async t=>{
  const {app,release,update}=await prepareUpdate(t);
  app.router.post('/api/projects/project-1/team-releases/old/git-update-preview',{...update,status:'unchanged',commit:release.payload.gitImport.commit,preview:null,changes:[]});
  app.document.querySelector('[data-tg-update-check]').click();await settle(5);
  assert.match(app.document.querySelector('[data-tg-update-result]').textContent,/aaaaaaaa/);
  assert.equal(app.document.querySelector('[data-tg-update-publish]').disabled,true);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/team-imports/publish')).length,0);
  app.router.get('/api/projects/project-1/team-releases/old',{...release,state:'revoked'});
  app.document.querySelector('[data-ti-release-detail]').click();await settle(5);
  assert.equal(app.document.querySelector('[data-tg-update]'),null);
  app.evaluate('state.currentUser={id:2,isPlatformAdmin:false};state.data.projects[0].ownerId=1');
  app.router.get('/api/projects/project-1/team-releases/old',release);
  app.document.querySelector('[data-ti-release-detail]').click();await settle(5);
  assert.equal(app.document.querySelector('[data-tg-update]'),null);
});

test('update confirmation is single-flight and cancellation sends no publication',async t=>{
  const {app,update}=await prepareUpdate(t);
  app.router.post('/api/projects/project-1/team-releases/old/git-update-preview',update);
  app.document.querySelector('[data-tg-update-check]').click();await settle(5);
  let confirm;app.window.updateConfirmation=new Promise(resolve=>{confirm=resolve;});app.evaluate('afConfirm=()=>updateConfirmation');
  const publish=app.document.querySelector('[data-tg-update-publish]');publish.click();publish.click();await settle(3);
  assert.equal(publish.disabled,true);
  const detail=app.document.querySelector('[data-ti-release-detail]');assert.equal(detail.disabled,true);
  const detailReads=()=>app.router.calls.filter(call=>call.method==='GET'&&call.path==='/api/projects/project-1/team-releases/old').length;
  const reads=detailReads();detail.click();await settle(2);assert.equal(detailReads(),reads);
  confirm(false);await settle(5);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/team-imports/publish')).length,0);
  assert.equal(publish.disabled,false);
  assert.equal(detail.disabled,false);
});

test('late branch preview cannot appear or be approved after project panel replacement',async t=>{
  const {app,update}=await prepareUpdate(t);
  let finish;app.router.post('/api/projects/project-1/team-releases/old/git-update-preview',()=>new Promise(resolve=>{finish=resolve;}));
  app.document.querySelector('[data-tg-update-check]').click();await settle(3);
  const oldPanel=app.document.querySelector('[data-tg-update]');
  app.document.querySelector('[data-ti-project]').remove();
  app.evaluate("document.body.insertAdjacentHTML('beforeend',teamIntelligencePanelHTML('project-2'))");
  finish(update);await settle(5);
  assert.equal(oldPanel.querySelector('[data-tg-update-result]').textContent,'');
  assert.equal(app.document.querySelector('[data-tg-update]'),null);
  assert.equal(app.evaluate("intelligenceView('project-1').gitUpdates.old.update"),null);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/team-imports/publish')).length,0);
});

test('replaced detail content invalidates a pending approval even when its outer host remains connected',async t=>{
  const {app,update}=await prepareUpdate(t);
  app.router.post('/api/projects/project-1/team-releases/old/git-update-preview',update);
  app.document.querySelector('[data-tg-update-check]').click();await settle(5);
  let confirm;app.window.replacedUpdateConfirmation=new Promise(resolve=>{confirm=resolve;});app.evaluate('afConfirm=()=>replacedUpdateConfirmation');
  app.document.querySelector('[data-tg-update-publish]').click();await settle(3);
  app.document.querySelector('[data-ti-release-content]').replaceChildren();
  confirm(true);await settle(5);
  assert.equal(app.document.querySelector('[data-ti-release-content]').isConnected,true);
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/team-imports/publish')).length,0);
});

test('refresh and reopened detail release their controls after discarding the old pending preview',async t=>{
  const {app,update}=await prepareUpdate(t);
  let finish;app.router.post('/api/projects/project-1/team-releases/old/git-update-preview',()=>new Promise(resolve=>{finish=resolve;}));
  app.document.querySelector('[data-tg-update-check]').click();await settle(3);
  await app.evaluate("loadTeamIntelligence('project-1')");
  app.document.querySelector('[data-ti-release-detail]').click();await settle(5);
  assert.equal(app.document.querySelector('[data-tg-update-check]').disabled,true);
  finish(update);await settle(5);
  assert.equal(app.document.querySelector('[data-tg-update-check]').disabled,false);
  assert.equal(app.document.querySelector('[data-ti-release-detail]').disabled,false);
  assert.equal(app.document.querySelector('[data-tg-update-publish]').disabled,true);
  assert.equal(app.document.querySelector('[data-tg-update-result]').textContent,'');
});
