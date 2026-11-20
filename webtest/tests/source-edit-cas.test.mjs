import test from 'node:test';
import assert from 'node:assert/strict';
import { loadApp, settle } from '../lib/load-app.mjs';

const first='1'.repeat(32),second='2'.repeat(32),third='3'.repeat(32);

test('source editor freezes its base, retains conflict draft and requires explicit comparison before a fresh save',async t=>{
  const app=await loadApp();t.after(app.close);
  const rule={id:'cas-rule',name:'Original',projectId:'project-1',content:'Original body',priority:1,sourceRevision:first};
  app.evaluate(`state.assetType='rules';state.data.rules=[${JSON.stringify(rule)}];state.selectedAsset='cas-rule';state.assetDetailKey='';renderAssets()`);
  const form=app.document.querySelector('#assetForm');form.elements.name.value='My unsaved name';form.elements.content.value='My unsaved body';app.evaluate('markAssetDirty()');
  app.evaluate(`state.data.rules[0]={...state.data.rules[0],name:'Other writer',content:'Other body',sourceRevision:'${second}'};renderAssets()`);
  assert.equal(form.dataset.sourceRevision,first);
  app.router.put('/api/rules',{error:'内容已被其他操作修改，本次更改未保存。请刷新并比较后重新提交',code:'source_edit_conflict'},409);
  await app.evaluate(`saveAsset({preventDefault(){},currentTarget:document.querySelector('#assetForm')})`);
  const writes=()=>app.router.calls.filter(c=>c.method==='PUT'&&c.path==='/api/rules');
  assert.equal(writes().length,1);assert.equal(writes()[0].options.headers['If-Match'],`"${first}"`);
  assert.equal(form.elements.name.value,'My unsaved name');assert.equal(form.elements.content.value,'My unsaved body');assert.equal(form.dataset.sourceRevision,first);
  const conflict=form.querySelector('[data-source-edit-conflict]');assert.ok(conflict);
  app.router.get('/api/rules',[{...rule,name:'Other writer',content:'Other body',sourceRevision:second}]);
  conflict.querySelectorAll('button')[0].click();await settle(4);
  assert.match(conflict.textContent,/Other body/);assert.match(conflict.textContent,/My unsaved body/);assert.equal(form.dataset.sourceRevision,first);assert.equal(writes().length,1);
  conflict.querySelectorAll('button')[1].click();assert.equal(form.dataset.sourceRevision,second);assert.equal(writes().length,1);
  app.router.put('/api/rules',call=>({...call.body,sourceRevision:third}));
  await app.evaluate(`saveAsset({preventDefault(){},currentTarget:document.querySelector('#assetForm')})`);
  assert.equal(writes().length,2);assert.equal(writes()[1].options.headers['If-Match'],`"${second}"`);assert.equal(app.evaluate('state.data.rules[0].sourceRevision'),third);
});

test('raw skill upload uses the pre-reading base and conflict leaves accepted manifest unchanged',async t=>{
  const app=await loadApp();t.after(app.close);
  app.evaluate(`state.assetType='skills';state.data.skills=[{id:'cas-upload',projectId:'project-1',name:'Skill',sourceRevision:'${first}',fileRefs:[]}];state.selectedAsset='cas-upload';state.assetDetailKey='';renderAssets()`);
  app.router.post('/api/skills/cas-upload/files',{error:'source changed',code:'source_edit_conflict'},409);
  const file={name:'SKILL.md',webkitRelativePath:'SKILL.md',size:7,arrayBuffer:async()=>{app.document.querySelector('#assetForm').dataset.sourceRevision=second;return new TextEncoder().encode('# Skill').buffer}};
  await app.evaluate('uploadSkillFiles').call(null,{files:[file],value:'chosen'});await settle(4);
  const calls=app.router.calls.filter(c=>c.path==='/api/skills/cas-upload/files'&&c.method==='POST');
  assert.equal(calls.length,1);assert.equal(calls[0].options.headers['If-Match'],`"${first}"`);assert.equal(calls[0].options.headers['Content-Type'],undefined);
  assert.equal(app.evaluate('state.data.skills[0].fileRefs.length'),0);assert.ok(app.document.querySelector('[data-source-edit-conflict]'));
  assert.equal(app.evaluate('state.skillPendingUpload.files.length'),1);
  app.router.get('/api/skills',[{id:'cas-upload',name:'Latest',sourceRevision:third}]);
  const conflict=app.document.querySelector('[data-source-edit-conflict]');
  conflict.querySelectorAll('button')[0].click();await settle(3);
  assert.equal(app.document.querySelector('[data-source-upload-retry]'),null);
  conflict.querySelectorAll('button')[1].click();
  assert.equal(app.document.querySelector('#assetForm').dataset.sourceRevision,third);
  assert.ok(app.document.querySelector('[data-source-upload-retry]'));
  assert.equal(app.router.calls.filter(c=>c.path==='/api/skills/cas-upload/files'&&c.method==='POST').length,1);
});

test('source comparison masks MCP secret fields and never retries or adopts a base automatically',async t=>{
  const app=await loadApp();t.after(app.close);
  app.evaluate(`state.data.mcpServers=[{id:'cas-mcp',name:'Custom',scope:'project',projectId:'project-1',transport:'http',url:'https://example.invalid',sourceRevision:'${first}'}];openCustomMCPModal(state.data.mcpServers[0]);document.querySelector('#customMcpForm').elements.name.value='Draft'`);
  app.router.put('/api/mcp-servers',{error:'source changed',code:'source_edit_conflict'},409);
  await app.evaluate(`saveCustomMCP({preventDefault(){},currentTarget:document.querySelector('#customMcpForm')})`);
  const form=app.document.querySelector('#customMcpForm');assert.equal(form.elements.name.value,'Draft');assert.equal(app.evaluate('state.editingMCPRevision'),first);
  app.router.get('/api/mcp-servers',[{id:'cas-mcp',name:'Latest',sourceRevision:second,headers:{Authorization:'MUST_NOT_RENDER_SECRET'}}]);
  form.querySelector('[data-source-edit-conflict] button').click();await settle(3);
  assert.doesNotMatch(form.textContent,/MUST_NOT_RENDER_SECRET/);assert.match(form.textContent,/敏感配置未展示/);assert.equal(app.evaluate('state.editingMCPRevision'),first);
  assert.equal(app.router.calls.filter(c=>c.method==='PUT'&&c.path==='/api/mcp-servers').length,1);
});
