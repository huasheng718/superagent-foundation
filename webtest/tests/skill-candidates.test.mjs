import test from 'node:test';
import assert from 'node:assert/strict';
import { TextEncoder, TextDecoder } from 'node:util';
import { loadApp, settle } from '../lib/load-app.mjs';
const base='/api/projects/project-1/skill-candidates';
const candidate={id:'candidate-1',projectId:'project-1',state:'quarantined',revision:1,sha256:'a'.repeat(64),skillId:'reviewed-skill',name:'Reviewed skill',createdBy:'member',license:'MIT',package:{skill:{id:'reviewed-skill',name:'Reviewed skill',files:{'SKILL.md':'---\nname: reviewed-skill\ndescription: Original instructions.\n---\n\n# ORIGINAL_SKILL\n','LICENSE':'ORIGINAL_LICENSE_NOTICE','nested/reference.md':'ORIGINAL_NESTED_BYTES'},fileRefs:[{path:'scripts/check.sh',mode:493,size:9,sha256:'b'.repeat(64)}]},binary:{'scripts/check.sh':'IyEvYmluL3No'},source:{repository:'https://example.com/team/source',commit:'1'.repeat(40),path:'skills/reviewed-skill'},publisher:'Original publisher',license:'MIT',noticePaths:['LICENSE']},resources:[{path:'nested/reference.md',size:21,mode:420,sha256:'c'.repeat(64)},{path:'scripts/check.sh',size:9,mode:493,sha256:'b'.repeat(64)}]};
async function setup(app,manage=true){
 app.window.TextEncoder=TextEncoder;app.window.TextDecoder=TextDecoder;
 app.router.get('/api/projects/project-1/team-releases',[]);app.router.get('/api/projects/project-1/memories',[]);app.router.get(base,[candidate]);app.router.get(base+'/candidate-1',candidate);
 app.evaluate(`state.currentUser={id:${manage?1:2},isPlatformAdmin:${manage}};state.data.projects[0].ownerId=1;document.body.insertAdjacentHTML('beforeend',teamIntelligencePanelHTML('project-1'))`);
 await app.evaluate("loadTeamIntelligence('project-1')");
}
test('candidate review shows raw instructions, nested digests, executable modes and license before approving current revision',async t=>{
 const app=await loadApp();t.after(app.close);await setup(app);
 const approve=app.document.querySelector('[data-ti-skill-decision="approved"]');assert.equal(approve.disabled,true);
 app.document.querySelector('[data-ti-skill-detail]').click();await settle(6);
 const text=app.document.querySelector('[data-ti-skill-content]').textContent;
 assert.match(text,/ORIGINAL_SKILL/);assert.match(text,/ORIGINAL_LICENSE_NOTICE/);assert.match(text,/ORIGINAL_NESTED_BYTES/);assert.match(text,/可执行/);assert.match(text,/cccccccccccccccc/);assert.match(text,/Original publisher/);
 assert.equal(approve.disabled,false);
 app.evaluate("afPrompt=async()=> 'Reviewed source, license and executable behavior'");
 app.router.post(base+'/candidate-1/decision',call=>{assert.deepEqual(call.body,{decision:'approved',expectedRevision:1,reason:'Reviewed source, license and executable behavior'});return {status:409,body:{code:'intelligence_conflict',error:'candidate revision changed'}};});
 approve.click();await settle(8);
 assert.match(app.document.querySelector('[data-ti-project]').textContent,/手动刷新后复核并重试/);
 assert.match(app.document.querySelector('[data-ti-project]').textContent,/隔离待审/);
 assert.equal(app.router.calls.filter(call=>call.method==='POST'&&call.path.endsWith('/decision')).length,1);
 assert.equal(app.router.calls.some(call=>call.method==='POST'&&call.path.endsWith('/team-releases')),false);
});
test('member can submit a package to quarantine but receives no approval control or fake success on rejection',async t=>{
 const app=await loadApp();t.after(app.close);await setup(app,false);
 assert.equal(app.document.querySelector('[data-ti-skill-decision]'),null);
 const field=app.document.querySelector('[data-ti-skill-package]');field.value=JSON.stringify(candidate.package);field.dispatchEvent(new app.window.Event('input',{bubbles:true}));
 app.router.post(base,call=>{assert.equal(call.body.skill.id,'reviewed-skill');return {status:400,body:{error:'binary digest mismatch'}};});
 app.document.querySelector('[data-ti-skill-submit]').click();await settle(8);
 assert.match(app.document.querySelector('[data-ti-project] [role="alert"]').textContent,/binary digest mismatch/);
 assert.equal(app.document.querySelector('[data-ti-skill-package]').value,JSON.stringify(candidate.package));
 assert.equal(app.document.querySelector('[data-ti-skill-decision]'),null);
});
test('candidate submission uses real server quarantine response and refreshes list without publishing formal configuration',async t=>{
 const app=await loadApp();t.after(app.close);await setup(app,false);
 let submitted=false;app.router.get(base,()=>submitted?[candidate]:[]);app.router.post(base,()=>{submitted=true;return candidate;});
 const field=app.document.querySelector('[data-ti-skill-package]');field.value=JSON.stringify(candidate.package);
 app.document.querySelector('[data-ti-skill-submit]').click();await settle(8);
 assert.equal(submitted,true);assert.equal(app.router.calls.filter(call=>call.method==='POST'&&call.path===base).length,1);
 assert.match(app.document.querySelector('[data-ti-project]').textContent,/隔离待审/);
 assert.equal(app.router.calls.some(call=>call.method!=='GET'&&call.path.endsWith('/team-releases')),false);
});
