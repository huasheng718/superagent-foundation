// Explicit local two-process source editing acceptance; no model or hosted IO.
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { randomUUID } from 'node:crypto';
import { writeFileSync } from 'node:fs';

test('CAP001 mutable definitions and file closures reject same-base edits across two HTTP processes',{
  skip:process.env.AGENTFLOW_E2E_SOURCE_EDIT_MULTI_INSTANCE!=='1'&&'requires disposable local two-process source CAS harness',timeout:90_000,
},async()=>{
  assert.equal(process.env.AGENTFLOW_E2E_LOCAL_MYSQL_CONTAINER,'superagent-p0-contribution');
  const bases=[process.env.AGENTFLOW_E2E_BASE,process.env.AGENTFLOW_E2E_SECOND_BASE],pids=[process.env.AGENTFLOW_E2E_PRIMARY_PID,process.env.AGENTFLOW_E2E_SECOND_PID].map(Number);
  for(const base of bases){const url=new URL(base);assert.equal(url.protocol,'http:');assert.equal(url.hostname,'127.0.0.1')}
  assert.notEqual(bases[0],bases[1]);assert.notEqual(pids[0],pids[1]);assert.ok(pids.every(pid=>pid>1));
  const token=process.env.AGENTFLOW_E2E_TOKEN;assert.ok(token);
  const request=async(index,method,path,body,headers={})=>{
    const multipart=body instanceof FormData;
    const response=await fetch(bases[index]+path,{method,headers:{Authorization:`Bearer ${token}`,...(!multipart&&body!==undefined?{'Content-Type':'application/json'}:{}),...headers},...(body===undefined?{}:{body:multipart?body:JSON.stringify(body)}),signal:AbortSignal.timeout(15_000)});
    const text=await response.text();let value;try{value=JSON.parse(text)}catch{value=null}return{status:response.status,value};
  };
  const match=value=>({'If-Match':`"${value.sourceRevision}"`});
  const own=[],suffix=randomUUID(),report={acceptance:'running',processIds:pids,apiBases:bases,definitions:[],fileRaces:[],cleanup:{},boundaries:{realHTTPProcesses:true,sharedMySQL:true,modelUsed:false,hostedWrites:false,physicalRemoteHosts:false}};
  const persist=()=>{if(process.env.AGENTFLOW_E2E_SOURCE_EDIT_REPORT)writeFileSync(process.env.AGENTFLOW_E2E_SOURCE_EDIT_REPORT,JSON.stringify(report,null,2)+'\n',{mode:0o600})};
  const current=async(kind,id)=>{
    const response=await request(0,'GET',kind==='agents'?`/api/agents/${id}`:`/api/${kind}`);assert.equal(response.status,200);
    return kind==='agents'?response.value:response.value.find(value=>value.id===id);
  };
  const race=async(call)=>{
    const result=await Promise.all([0,1].map(call));assert.deepEqual(result.map(r=>r.status).sort(),[200,409]);assert.equal(result.find(r=>r.status===409).value.code,'source_edit_conflict');return result;
  };
  try{
    for(const[index,pid]of pids.entries()){const health=await request(index,'GET','/api/health');assert.equal(health.status,200);assert.equal(health.value.pid,pid);assert.equal((await request(index,'GET','/api/auth/current')).status,200)}
    for(const kind of ['agents','rules','skills','mcp-servers']){
      const id=`e2e-cas-${kind}-${suffix}`,value={id,name:'Original fixture',description:'Disposable source CAS acceptance',projectId:'e2e-auto'};
      if(kind==='agents')Object.assign(value,{prompt:'FROZEN_ORIGINAL_AGENT',version:1,executorId:'codex',rules:[],skills:[],mcpServers:[],verification:[]});
      if(kind==='rules')Object.assign(value,{content:'FROZEN_ORIGINAL_RULE',priority:1});
      if(kind==='mcp-servers')Object.assign(value,{scope:'project',transport:'http',url:'https://example.invalid/not-called'});
      const created=await request(0,'PUT',`/api/${kind}`,value,{'If-None-Match':'*'});assert.equal(created.status,200);own.push({kind,id});assert.ok(created.value.sourceRevision);
      assert.equal((await request(1,'PUT',`/api/${kind}`,{...created.value,name:'unguarded overwrite'})).status,428);
      const base=created.value,result=await race(index=>request(index,'PUT',`/api/${kind}`,{...base,name:`Writer ${index}`,...(kind==='agents'?{version:2}:{})},match(base)));
      const winner=result.find(r=>r.status===200).value,accepted=await current(kind,id);
      assert.equal(accepted.name,winner.name);assert.equal(accepted.sourceRevision,winner.sourceRevision);assert.notEqual(accepted.sourceRevision,base.sourceRevision);
      report.definitions.push({kind,id,statuses:result.map(r=>r.status),baseRevision:base.sourceRevision,acceptedRevision:winner.sourceRevision,acceptedName:winner.name,missingPreconditionStatus:428});persist();
      if(['skills','mcp-servers'].includes(kind)){
        const upload=()=>{const body=new FormData();body.append('files',new Blob(['# Source CAS fixture\n']),'SKILL.md');body.append('paths','SKILL.md');return body};
        const fileResults=await race(index=>index===0?request(index,'POST',`/api/${kind}/${id}/files`,upload(),match(accepted)):request(index,'PUT',`/api/${kind}`,{...accepted,name:'Metadata competitor'},match(accepted)));
        const after=await current(kind,id),fileWon=fileResults[0].status===200;
        assert.equal(after.sourceRevision,fileResults.find(r=>r.status===200).value.sourceRevision);assert.equal(after.fileRefs?.length||0,fileWon?1:0);
        if(fileWon){const file=await fetch(`${bases[1]}/api/${kind}/${id}/files/SKILL.md`,{headers:{Authorization:`Bearer ${token}`}});assert.equal(file.status,200);assert.equal(await file.text(),'# Source CAS fixture\n')}
        report.fileRaces.push({kind,id,statuses:fileResults.map(r=>r.status),fileReplacementWon:fileWon,manifestCount:after.fileRefs?.length||0,acceptedRevision:after.sourceRevision});persist();
      }
      if(kind==='agents'){
        const history=await request(0,'GET',`/api/agents/${id}/revisions`);assert.equal(history.status,200);assert.equal(history.value.length,1);
        const restore=await race(index=>request(index,'POST',`/api/agents/${id}/revisions/${history.value[0].id}/restore`,{},match(accepted)));
        const restored=await current(kind,id);assert.equal(restored.prompt,'FROZEN_ORIGINAL_AGENT');assert.equal(restored.version,3);assert.notEqual(restored.sourceRevision,base.sourceRevision);
        assert.equal((await request(0,'GET',`/api/agents/${id}/revisions`)).value.length,2);
        report.agentRestore={statuses:restore.map(r=>r.status),historyCount:2,restoredRevision:restored.sourceRevision,restoredVersion:restored.version};
      }
    }
    const mcp=own.find(value=>value.kind==='mcp-servers'),fresh=`e2e-cas-import-${suffix}`;
    const imported=await request(0,'POST','/api/mcp-servers/import',{mcpServers:[{id:fresh,name:'Fresh batch item',projectId:'e2e-auto',scope:'project',transport:'http',url:'https://example.invalid'},{id:mcp.id,name:'Collision',projectId:'e2e-auto',scope:'project',transport:'http',url:'https://example.invalid'}]});
    assert.equal(imported.status,409);assert.equal(await current('mcp-servers',fresh),undefined);report.createOnlyImportRollback=true;
    report.acceptance='passed';
  }catch(error){report.acceptance='failed';throw error}
  finally{
    const errors=[];
    for(const{kind,id}of own.reverse())try{const value=await current(kind,id);if(!value)continue;const response=await request(0,'DELETE',kind==='agents'?`/api/agents/${id}`:`/api/${kind}?id=${id}`,undefined,match(value));assert.equal(response.status,kind==='mcp-servers'?200:204)}catch(error){errors.push(error.message)}
    report.cleanup={ownedDefinitionsDeleted:errors.length===0,count:own.length,errors};if(errors.length)report.acceptance='failed';persist();assert.deepEqual(errors,[]);
  }
});
