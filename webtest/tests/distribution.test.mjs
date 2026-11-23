import test from 'node:test';
import assert from 'node:assert/strict';
import { loadApp, settle } from '../lib/load-app.mjs';
const first='1'.repeat(32),second='2'.repeat(32),third='3'.repeat(32);

test('distribution is mounted on rule detail and saves an explicit decision without rebasing the content draft',async t=>{
 const app=await loadApp();t.after(app.close);
 const item={id:'policy-rule',name:'Rule',projectId:'project-1',content:'original',sourceRevision:first};
 app.evaluate(`state.authRequired=false;state.assetType='rules';state.data.rules=[${JSON.stringify(item)}];state.selectedAsset='policy-rule';state.assetDetailKey='';renderAssets()`);
 const content=app.document.querySelector('#assetForm');content.elements.name.value='Unsaved content name';
 const open=app.document.querySelector('[data-distribution-open="rules"]');assert.ok(open);open.click();
 const form=app.document.querySelector('[data-distribution-form]');form.elements.copy.value='deny';form.elements.edit.value='allow';form.elements.export.value='deny';form.elements.reason.value='Owner reviewed copying';
 app.router.put('/api/rules/policy-rule/distribution',{kind:'rule',id:item.id,sourceRevision:second,distribution:{copy:'deny',edit:'allow',export:'deny'},effectiveDistribution:{copy:'deny',edit:'allow',export:'deny'}});
 await app.evaluate(`document.querySelector('[data-distribution-form]').onsubmit({preventDefault(){}})`);
 const call=app.router.calls.find(call=>call.path==='/api/rules/policy-rule/distribution');
 assert.equal(call.options.headers['If-Match'],`"${first}"`);assert.deepEqual(call.body,{policy:{copy:'deny',edit:'allow',export:'deny'},reason:'Owner reviewed copying'});
 assert.equal(content.dataset.sourceRevision,first);assert.equal(content.elements.name.value,'Unsaved content name');assert.equal(app.evaluate('state.data.rules[0].sourceRevision'),second);
 assert.match(app.document.querySelector('[data-distribution-status]').textContent,/已保存/);
});

test('distribution conflicts preserve selections and reason, and require explicit read and adoption before retry',async t=>{
 const app=await loadApp();t.after(app.close);
 const item={id:'policy-skill',name:'Skill',projectId:'project-1',sourceRevision:first};
 app.evaluate(`state.authRequired=false;state.data.skills=[${JSON.stringify(item)}];openDistributionDialog('skills',state.data.skills[0])`);
 const form=app.document.querySelector('[data-distribution-form]');form.elements.copy.value='deny';form.elements.reason.value='Keep this draft';
 let writes=0;app.router.put('/api/skills/policy-skill/distribution',()=>++writes===1?{status:409,body:{code:'source_edit_conflict',error:'changed'}}:{id:item.id,sourceRevision:third,distribution:{copy:'deny',edit:'allow',export:'allow'}});
 await app.evaluate(`document.querySelector('[data-distribution-form]').onsubmit({preventDefault(){}})`);
 assert.equal(form.elements.copy.value,'deny');assert.equal(form.elements.reason.value,'Keep this draft');assert.equal(writes,1);
 app.router.get('/api/skills',[{...item,sourceRevision:second,distribution:{copy:'allow',edit:'allow',export:'allow'}}]);
 app.document.querySelector('[data-distribution-load]').click();await settle(4);assert.equal(writes,1);
 assert.equal(app.document.querySelector('[data-distribution-save]').disabled,true);
 app.document.querySelector('[data-distribution-adopt]').click();assert.equal(writes,1);assert.equal(form.elements.copy.value,'deny');
 await app.evaluate(`document.querySelector('[data-distribution-form]').onsubmit({preventDefault(){}})`);
 const calls=app.router.calls.filter(call=>call.path==='/api/skills/policy-skill/distribution');assert.equal(calls.length,2);assert.equal(calls[1].options.headers['If-Match'],`"${second}"`);
});

test('inherited denials cannot be relaxed and a nonowner sees inspection only',async t=>{
 const app=await loadApp();t.after(app.close);
 const item={id:'policy-owned',name:'Inherited',projectId:'project-1',sourceRevision:first,distributionOrigins:[{kind:'skill',id:'source',projectId:'elsewhere',sourceRevision:first,sha256:'a'.repeat(64),policy:{copy:'allow',edit:'deny',export:'deny'}}]};
 app.evaluate(`state.authRequired=false;openDistributionDialog('skills',${JSON.stringify(item)})`);
 const form=app.document.querySelector('[data-distribution-form]');assert.equal(form.elements.edit.querySelector('[value="allow"]').disabled,true);assert.match(app.document.querySelector('[data-distribution-origins]').textContent,/source/);
 form.elements.edit.value='allow';form.elements.reason.value='try relaxing';await app.evaluate(`document.querySelector('[data-distribution-form]').onsubmit({preventDefault(){}})`);
 assert.match(app.document.querySelector('[data-distribution-status]').textContent,/不能放宽/);assert.equal(app.router.calls.filter(call=>call.method==='PUT').length,0);
 app.evaluate(`state.authRequired=true;state.currentUser={id:999};state.data.projects=[{id:'project-1',ownerId:1}];openDistributionDialog('skills',${JSON.stringify(item)})`);
 assert.equal(app.document.querySelector('[data-distribution-save]'),null);assert.match(app.document.querySelector('#distributionDialog').textContent,/可保存/);
});
