import test from 'node:test';
import assert from 'node:assert/strict';
import {loadApp,settle} from '../lib/load-app.mjs';

const proposal={id:'1'.repeat(64),projectId:'project-1',repositoryId:'own',providerProject:'fixture/config',releaseId:'formal',sha256:'2'.repeat(64),revision:1,state:'pending_review',baseCommit:'a'.repeat(40),resultSHA:'b'.repeat(40),targetRef:'refs/heads/main',patch:'reviewed binary patch',patchSha256:'3'.repeat(64),validation:{policy:'teamai-configuration-proposal-v1',runGatesVerified:false,remoteTargetAtomicCAS:false,checks:['raw Git tree checked']},files:[{path:'rules/check.md',before:{mode:'100644',size:3,sha256:'4'.repeat(64)},after:{mode:'100755',size:4,sha256:'5'.repeat(64)}}]};
async function prepare(t){
  const app=await loadApp();t.after(app.close);
  app.router.get('/api/projects/project-1/team-releases',[]);
  app.router.get('/api/projects/project-1/memories',[]);
  app.router.get('/api/projects/project-1/skill-candidates',[]);
  app.evaluate(`state.currentUser={id:1,isPlatformAdmin:true};document.body.insertAdjacentHTML('beforeend',teamIntelligencePanelHTML('project-1'))`);
  await app.evaluate("loadTeamIntelligence('project-1')");await settle(3);
  app.evaluate('afConfirm=async()=>true');
  return app;
}
async function review(app,value=proposal){
  const base='/api/projects/project-1/team-git-proposals';
  app.router.get(base,[value]);app.router.get(`${base}/${value.id}`,value);
  app.document.querySelector('[data-tp-refresh]').click();await settle(4);
  app.document.querySelector('[data-tp-id]').click();await settle(4);
}

test('proposal review binds approval and remote command to distinct exact revisions',async t=>{
  const app=await prepare(t),base='/api/projects/project-1/team-git-proposals';
  assert.equal(app.router.calls.some(call=>call.path.includes('team-git-proposals')),false);
  await review(app);
  assert.match(app.document.querySelector('[data-tp-detail]').textContent,/100755/);
  assert.match(app.document.querySelector('[data-tp-detail]').textContent,/未通过执行任务/);
  const approved={...proposal,revision:2,state:'approved',publication:{state:'ready',prCreateAttempted:false}};
  app.router.post(`${base}/${proposal.id}/decision`,call=>{
    assert.deepEqual(call.body,{expectedRevision:1,expectedProposalSha256:proposal.sha256,reason:'reviewed exact source and modes',decision:'approved'});
    app.router.get(`${base}/${proposal.id}`,approved);return approved;
  });
  const reason=app.document.querySelector('[data-tp-reason]');reason.value='reviewed exact source and modes';reason.dispatchEvent(new app.window.Event('input'));
  app.document.querySelector('[data-tp-action="approved"]').click();await settle(6);
  assert.equal(app.router.calls.some(call=>call.path.endsWith('/publish')),false);
  app.router.post(`${base}/${proposal.id}/publish`,call=>{
    assert.deepEqual(call.body,{expectedRevision:2,expectedProposalSha256:proposal.sha256,expectedBaseCommit:proposal.baseCommit});
    return {status:409,body:{error:'create outcome uncertain',code:'team_git_proposal_conflict'}};
  });
  app.document.querySelector('[data-tp-action="publish"]').click();await settle(6);
  assert.match(app.document.querySelector('[data-tp-detail]').textContent,/create outcome uncertain/);
  assert.equal(app.document.querySelector('[data-tp-action="publish"]').disabled,true);
  assert.equal(app.document.querySelector('[data-tp-reason]').value,'reviewed exact source and modes');
  assert.equal(app.router.calls.filter(call=>call.path.endsWith('/publish')).length,1);
  const uncertain={...approved,publication:{state:'publishing',prCreateAttempted:true}};
  app.router.get(`${base}/${proposal.id}`,uncertain);
  app.document.querySelector('[data-tp-read]').click();await settle(5);
  assert.equal(app.document.querySelector('[data-tp-action="publish"]'),null);
  assert.ok(app.document.querySelector('[data-tp-action="reconcile"]'));
  assert.match(app.document.querySelector('[data-tp-detail]').textContent,/不会重发创建请求/);
});

test('replaced review panel cannot submit an already open approval confirmation',async t=>{
  const app=await prepare(t);await review(app);
  let confirm;
  app.window.holdConfirmation=()=>new Promise(resolve=>{confirm=resolve;});
  app.evaluate('afConfirm=()=>window.holdConfirmation()');
  const reason=app.document.querySelector('[data-tp-reason]');reason.value='reviewed';reason.dispatchEvent(new app.window.Event('input'));
  app.document.querySelector('[data-tp-action="approved"]').click();await settle(3);
  await app.evaluate("loadTeamIntelligence('project-1')");
  confirm(true);await settle(5);
  assert.equal(app.router.calls.some(call=>call.path.endsWith('/decision')&&call.path.includes('team-git-proposals')),false);
  assert.equal(app.document.querySelector('[data-tp-refresh]').disabled,false);
});

test('non-admin owner can review configuration without a controller publish button',async t=>{
  const app=await prepare(t);await review(app,{...proposal,state:'approved',revision:2});
  app.evaluate('state.currentUser={id:1,isPlatformAdmin:false};state.data.projects[0].ownerId=1;renderTeamIntelligence("project-1")');
  assert.equal(app.document.querySelector('[data-tp-action="publish"]'),null);
  assert.ok(app.document.querySelector('[data-tp-read]'));
  app.evaluate('state.currentUser={id:2,isPlatformAdmin:false};renderTeamIntelligence("project-1")');
  assert.equal(app.document.querySelector('[data-team-proposals]'),null);
});

test('foreign proposal response never enables a decision',async t=>{
  const app=await prepare(t),base='/api/projects/project-1/team-git-proposals';
  app.router.get(base,[proposal]);app.router.get(`${base}/${proposal.id}`,{...proposal,projectId:'foreign'});
  app.document.querySelector('[data-tp-refresh]').click();await settle(4);
  app.document.querySelector('[data-tp-id]').click();await settle(4);
  assert.equal(app.document.querySelector('[data-tp-action="approved"]'),null);
  assert.match(app.document.querySelector('[data-tp-detail]').textContent,/缺少可核对/);
});

test('saving a proposal uses reviewed binary replacement and never auto-approves',async t=>{
  const app=await prepare(t),base='/api/projects/project-1';
  const release={id:'formal',state:'published',sha256:'6'.repeat(64),payload:{gitImport:{files:[{path:'rules/check.md',mode:'100644'}]}}};
  app.evaluate('intelligenceView("project-1").releases=[{id:"formal",state:"published"}];renderTeamIntelligence("project-1")');
  app.router.get(`${base}/team-releases/formal`,release);
  app.router.post(`${base}/team-releases/formal/git-export-preview`,{baseCommit:proposal.baseCommit,patchSha256:proposal.patchSha256,patch:proposal.patch,proposed:{}});
  app.document.querySelector('[data-ti-release-detail]').click();await settle(5);
  const upload=app.document.querySelector('[data-tg-export-file]');Object.defineProperty(upload,'files',{value:[{size:3,arrayBuffer:async()=>Uint8Array.from([0,255,128]).buffer}]});
  app.document.querySelector('[data-tg-export-preview]').click();await settle(5);
  app.router.post(`${base}/team-git-proposals`,call=>{
    assert.deepEqual(call.body,{releaseId:'formal',expectedReleaseSha256:release.sha256,expectedBaseCommit:proposal.baseCommit,expectedPatchSha256:proposal.patchSha256,changes:[{path:'rules/check.md',content:'AP+A',executable:false}]});return proposal;
  });
  app.document.querySelector('[data-tg-proposal-prepare]').click();await settle(5);
  assert.match(app.document.querySelector('[data-tp-detail]').textContent,/reviewed binary patch/);
  assert.equal(app.router.calls.some(call=>call.path.endsWith('/decision')||call.path.endsWith('/publish')),false);
  upload.dispatchEvent(new app.window.Event('change'));
  assert.equal(app.document.querySelector('[data-tg-proposal-prepare]'),null);
});
