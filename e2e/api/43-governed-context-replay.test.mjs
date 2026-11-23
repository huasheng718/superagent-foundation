// Opt-in real Codex materialization and immutable handoff; no hosted writes.
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createHash } from 'node:crypto';
import { readFileSync, statSync } from 'node:fs';
import { cfg, uid } from '../lib/env.mjs';
import { api, get, post, put, del, list, waitFor, track } from '../lib/api.mjs';
import { agentNode, script, approval, createAgent, createWorkflow, trackRun } from '../lib/fixtures.mjs';
import { runContributionHandoff } from '../lib/contribution-handoff.mjs';

const credentialFile=process.env.AGENTFLOW_E2E_CODEX_CREDENTIAL_FILE||'';
const enabled=process.env.AGENTFLOW_E2E_HANDOFF==='1'&&Boolean(credentialFile)
  &&process.env.AGENTFLOW_E2E_LOCAL_MYSQL_CONTAINER==='superagent-p0-contribution'
  &&['127.0.0.1','localhost'].includes(new URL(cfg.base).hostname)&&process.platform==='darwin';
const hash=value=>createHash('sha256').update(value).digest('hex');

test('CAP002 actual fresh Codex Runner consumes frozen release after catalog mutation',{
  skip:!enabled&&'requires explicit disposable local platform and private model credential file',timeout:600_000,
},async t=>{
  assert.equal(statSync(credentialFile).mode&0o077,0,'credentials must remain owner-only');
  const credentials=JSON.parse(readFileSync(credentialFile,'utf8'));
  assert.ok(credentials.key&&credentials.baseUrl&&credentials.model);
  await runContributionHandoff(t,{phase:'receipts',entryName:'43-governed-context-replay.test.mjs',entrySource:readFileSync(new URL(import.meta.url)),
    sensitiveValues:[credentials.key],sensitiveEndpoints:[credentials.baseUrl],
    onWritersReady:async({report,repositoryId,baseSHA,persist,onBeforeCleanup,sessions})=>{
      const mac=report.runners.find(r=>r.kind==='mac'),linux=report.runners.find(r=>r.kind==='linux');
      const user=list(await get('/api/users')).find(u=>u.loginName==='e2e-local');assert.ok(user);
      await put(`/api/users/${user.id}/credentials`,{codexApiKey:credentials.key,codexBaseUrl:credentials.baseUrl});
      await put('/api/runners/runtimes',{runnerId:mac.id,provider:'codex',authMode:'personal'});
      await put('/api/runners/default',{runnerId:mac.id});
      const skillId=uid('pinned-skill'),ruleId=uid('pinned-rule');
      const skillContent=`---\nname: ${skillId}\ndescription: Read the frozen nested and binary fixtures.\n---\n\n# PINNED_SKILL_V1\nRead nested/resource.txt and nested/data.bin without modification.\n`;
      const resource='PINNED_NESTED_RESOURCE_V1\n',binary=Buffer.from([0,255,128,13,10,1]),rule='PINNED_RULE_V1: do not change the supplied resources.\n';
      const skill={id:skillId,name:'Immutable resources',description:'Read the frozen nested and binary fixtures.',projectId:cfg.projectId,content:skillContent,files:{'nested/resource.txt':resource,'NOTICE.txt':'Disposable test fixture, created for this acceptance test.\n'}};
      let savedSkill=await put('/api/skills',skill,{headers:{'If-None-Match':'*'}});
      const match=value=>({headers:{'If-Match':JSON.stringify(value.sourceRevision)}});
      track(`skill ${skillId}`,()=>del(`/api/skills/${skillId}`,match(savedSkill)));
      const upload=async(bytes,changed=false)=>{const body=new FormData();for(const [path,data] of [['SKILL.md',changed?'# LATEST_UNAPPROVED_SKILL':skillContent],['nested/resource.txt',changed?'LATEST_UNAPPROVED_RESOURCE':resource],['nested/data.bin',bytes],['NOTICE.txt','Disposable acceptance fixture.\n']]){body.append('files',new Blob([data]),path.split('/').at(-1));body.append('paths',path);}savedSkill=await api('POST',`/api/skills/${skillId}/files`,body,match(savedSkill));};
      await upload(binary);
      let savedRule=await put('/api/rules',{id:ruleId,name:'Pinned rule',projectId:cfg.projectId,content:rule},{headers:{'If-None-Match':'*'}});
      track(`rule ${ruleId}`,()=>del(`/api/rules/${ruleId}`,match(savedRule)));
      const reader=`python3 - <<'PY'
import pathlib,hashlib,json,subprocess
root=pathlib.Path.cwd()
repos=([root] if (root/'.git').exists() else [])+[p.parent for p in root.glob('*/.git')]
assert len(repos)==1
repo=repos[0]
skill=(root/'.agentflow/skills/${skillId}/SKILL.md').read_bytes()
resource=(root/'.agentflow/skills/${skillId}/nested/resource.txt').read_bytes()
binary=(root/'.agentflow/skills/${skillId}/nested/data.bin').read_bytes()
rule=(root/'.agentflow/rules/${ruleId}.md').read_bytes()
systems=[p.read_text() for p in (root/'.agentflow/config').glob('*/agent-system.md')]
h=lambda b:hashlib.sha256(b).hexdigest()
data={'skillContent':skill.decode(),'resourceText':resource.decode(),'binaryHex':binary.hex(),'ruleContent':rule.decode(),'skillSHA256':h(skill),'resourceSHA256':h(resource),'binarySHA256':h(binary),'ruleSHA256':h(rule),'memorySeen':any('PINNED_APPROVED_MEMORY_V1' in text for text in systems),'baseSHA':subprocess.check_output(['git','-C',str(repo),'rev-parse','HEAD'],text=True).strip()}
(root/'reports').mkdir(exist_ok=True)
(repo/'governed').mkdir(exist_ok=True)
raw=json.dumps(data,sort_keys=True)
(root/'reports/governed.json').write_text(raw)
(repo/'governed/context-result.json').write_text(raw)
print('ACTUAL_FROZEN_CONTEXT_READ')
PY`;
      const prompt=`Execute exactly the following foreground command in this disposable workspace. It reads only the approved role's fixed skills/rules and writes two reports. Do not install packages, use network, modify other files, or run Git write operations. After command completion submit the normal node result. PINNED_ROLE_V1.\n${reader}`;
      const agentId=await createAgent('governed-reader',prompt,{executorId:'codex',model:credentials.model,rules:[ruleId],skills:[skillId],loop:{enabled:false}});
      const base=`/api/projects/${cfg.projectId}`;
      const releases=list(await get(base+'/team-releases'));
      const release=await post(base+'/team-releases',{name:'CAP002 immutable fixture',agentIds:[agentId],expectedBaseVersion:Math.max(0,...releases.map(r=>r.version))});
      let memory=await post(base+'/memories',{title:'CAP002 reviewed fixture',content:'PINNED_APPROVED_MEMORY_V1',source:{path:'e2e/api/43-governed-context-replay.test.mjs'}});
      memory=await post(`${base}/memories/${memory.id}/decision`,{decision:'approved',expectedRevision:memory.revision,reason:'Disposable deterministic fixture reviewed by the authenticated test owner; not human business acceptance.'});
      track(`memory ${memory.id}`,async()=>{const current=await get(`${base}/memories`);const m=list(current).find(v=>v.id===memory.id);if(m?.state==='approved')await post(`${base}/memories/${m.id}/decision`,{decision:'revoked',expectedRevision:m.revision,reason:'Acceptance fixture cleanup'});});
      track(`release ${release.id}`,async()=>{const current=await get(`${base}/team-releases/${release.id}`);if(current.state==='published')await post(`${base}/team-releases/${release.id}/revoke`,{expectedRevision:current.revision,reason:'Acceptance fixture cleanup'});});
      const expected={skillContent,resourceText:resource,binaryHex:binary.toString('hex'),ruleContent:rule,skillSHA256:hash(skillContent),resourceSHA256:hash(resource),binarySHA256:hash(binary),ruleSHA256:hash(rule),memorySeen:true,baseSHA};
      const verifier=`set -eu\npython3 - <<'PY'\nimport pathlib,json,hashlib,subprocess\nroot=pathlib.Path.cwd()\nrepos=([root] if (root/'.git').exists() else [])+[p.parent for p in root.glob('*/.git')]\nassert len(repos)==1\nrepo=repos[0]\np=repo/'governed/context-result.json'\ndata=json.loads(p.read_text())\nexpected=json.loads(${JSON.stringify(JSON.stringify(expected))})\nfor key,value in expected.items(): assert data[key]==value,(key,data[key],value)\nhead=subprocess.check_output(['git','-C',str(repo),'rev-parse','HEAD'],text=True).strip()\n(root/'reports').mkdir(exist_ok=True)\n(root/'reports/verified.json').write_text(json.dumps({'headSHA':head,'reportSHA256':hashlib.sha256(p.read_bytes()).hexdigest(),'allFrozenInputsVerified':True}))\nprint('FROZEN_CONTEXT_VERIFIED')\nPY`;
      const workflowId=await createWorkflow('governed-replay',[approval('before',''),agentNode('consume',agentId,{executorId:'codex',maxDurationSec:150,artifactOutputs:[{name:'governed-report',path:'reports/governed.json',required:true,mediaType:'application/json'}]}),script('verify',verifier,{artifactOutputs:[{name:'verified',path:'reports/verified.json',required:true,mediaType:'application/json'}]}),approval('after','')],{sandboxProfileId:'local'});
      const placement=r=>({runnerId:r.id,os:r.os,arch:r.arch});
      const run=await post('/api/runs',{workflowId,projectId:cfg.projectId,input:'Verify frozen release bytes on a fresh Runner; no hosted writes',repositoryIds:[repositoryId],runnerWorkspaceLayout:'attempt',workspaceBaseSHAs:{[repositoryId]:baseSHA},nodePlacements:{consume:placement(mac),verify:placement(linux)},teamContext:{releaseId:release.id,memoryIds:[memory.id]}});trackRun(run.id);
      report.governedContextReplay={acceptance:'running',runId:run.id,releaseId:release.id,baseSHA};persist();
      onBeforeCleanup(async()=>{const current=await get(`/api/runs/${run.id}`);const tasks=list(await get('/api/runners/tasks')).filter(task=>task.runId===run.id);report.governedReplaySnapshot={run:current,tasks,events:[]};for(const task of tasks){const token=sessions.get(task.runnerId||task.leaseOwner);if(token)report.governedReplaySnapshot.events.push({taskId:task.id,events:await get(`/api/runners/tasks/${task.id}/events`,{auth:false,headers:{Authorization:`Bearer ${token}`}}).catch(error=>({error:error.message}))});}persist();});
      await waitFor('initial context freeze gate',async()=>{const r=await get(`/api/runs/${run.id}`);return r.nodes?.before?.status==='waiting_approval'?r:null;},{timeoutMs:45_000,intervalMs:300});
      const fixedSHA=run.intelligence.sha256;
      const liveAgent=list(await get('/api/agents')).find(a=>a.id===agentId);assert.ok(liveAgent);
      await put('/api/agents',{...liveAgent,prompt:'LATEST_UNAPPROVED_ROLE: fail if consumed',rules:[],skills:[]},match(liveAgent));
      savedRule=await put('/api/rules',{id:ruleId,name:'Changed rule',projectId:cfg.projectId,content:'LATEST_UNAPPROVED_RULE'},match(savedRule));
      savedSkill=await put('/api/skills',{...savedSkill,content:'# LATEST_UNAPPROVED_SKILL',files:{'nested/resource.txt':'LATEST_UNAPPROVED_RESOURCE'}},match(savedSkill));await upload(Buffer.from('LATEST_UNAPPROVED_BINARY'),true);
      await post(`/api/runs/${run.id}/approve`,{nodeId:'before',context:'Automated immutable-context acceptance; live catalog has deliberately changed.'});
      const waiting=await waitFor('actual Codex and Linux frozen context verification',async()=>{const r=await get(`/api/runs/${run.id}`);if(['failed','cancelled'].includes(r.status))throw new Error(`context replay failed: ${JSON.stringify(r.nodes)}`);return r.nodes?.after?.status==='waiting_approval'?r:null;},{timeoutMs:240_000,intervalMs:500});
      assert.equal(waiting.intelligence.sha256,fixedSHA);
      const tasks=list(await get('/api/runners/tasks')).filter(task=>task.runId===run.id&&['consume','verify'].includes(task.nodeId));assert.equal(tasks.length,2);
      const author=tasks.find(t=>t.nodeId==='consume'),verification=tasks.find(t=>t.nodeId==='verify');
      assert.equal(author.invocation.governedContext.intelligenceSHA256,fixedSHA);assert.equal(author.invocation.governedContext.releaseId,release.id);assert.equal(author.outputs.contextEvidence.intelligenceSHA256,fixedSHA);assert.equal(author.outputs.contextEvidence.attempt,author.attempt);assert.equal(author.outputs.contextEvidence.contextSHA256,author.invocation.workspaceIsolation.contextSHA256);assert.equal(author.acceptedContributions[0].contextSHA256,author.outputs.contextEvidence.contextSHA256);
      const measured=author.outputs.contextEvidence.files;assert.ok(measured.some(f=>f.resourceId===skillId&&f.path.endsWith('nested/data.bin')&&f.sha256===hash(binary)));assert.ok(measured.some(f=>f.resourceId===skillId&&f.path.endsWith('SKILL.md')&&f.sourceSHA256===hash(skillContent)&&f.sha256===hash(skillContent)));
      assert.equal(author.provider,'codex');assert.equal(author.status,'success');assert.equal(author.invocation.credentialSource,'personal');assert.equal(author.invocation.agent.prompt,prompt);
      assert.equal(author.invocation.assets.skills[0].content,skillContent);
      assert.equal(Buffer.from(author.invocation.assets.skillFiles[skillId]['nested/data.bin'],'base64').toString('hex'),binary.toString('hex'));
      assert.equal(author.acceptedContributions.length,1);assert.equal(verification.acceptedContributions[0].resultSHA,author.acceptedContributions[0].resultSHA);
      assert.equal(verification.invocation.contributionInputs[0].headSHA,author.acceptedContributions[0].resultSHA);
      const artifact=waiting.artifacts.find(a=>a.name==='governed-report');assert.ok(artifact);
      const raw=await api('GET',`/api/runs/${run.id}/artifacts/${encodeURIComponent(artifact.id)}`,undefined,{raw:true});const actual=JSON.parse(raw.text);
      assert.equal(actual.skillSHA256,hash(skillContent));assert.equal(actual.binaryHex,binary.toString('hex'));assert.equal(actual.memorySeen,true);assert.equal(actual.baseSHA,baseSHA);
      report.governedContextReplay={acceptance:'passed',modelUsed:true,runId:run.id,releaseId:release.id,releaseSHA256:release.sha256,memoryId:memory.id,memoryRevision:memory.revision,intelligenceSHA256:fixedSHA,baseSHA,resultSHA:author.acceptedContributions[0].resultSHA,artifact:{id:artifact.id,sha256:artifact.sha256},effectiveResourceDigests:{skillSHA256:actual.skillSHA256,resourceSHA256:actual.resourceSHA256,binarySHA256:actual.binarySHA256,ruleSHA256:actual.ruleSHA256},tasks:tasks.map(t=>({id:t.id,nodeId:t.nodeId,attempt:t.attempt,status:t.status,provider:t.provider,contextSHA256:t.invocation.workspaceIsolation.contextSHA256,governedContext:t.invocation.governedContext,contextEvidence:t.outputs?.contextEvidence,acceptedContributions:t.acceptedContributions})),boundaries:{catalogMutatedBeforeInvocation:true,automaticPublication:false,sourceAndMaterializedSkillEqual:'fixture has canonical frontmatter; arbitrary SKILL.md may normalize',humanApproval:'API owner test authorization; not human business approval'}};persist();
      await post(`/api/runs/${run.id}/approve`,{nodeId:'after',context:'Automated acceptance complete; no hosted publication authorized.'});
      await waitFor('governed Run completion and Worker cleanup',async()=>{const r=await get(`/api/runs/${run.id}`);return r.status==='succeeded'&&Date.parse(r.workspaceCleanedAt)>0?r:null;},{timeoutMs:60_000,intervalMs:500});
    },
  });
});
