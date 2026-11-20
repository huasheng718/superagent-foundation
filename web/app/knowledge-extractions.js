// Completed model outputs enter quarantine; importing never approves knowledge.
function knowledgeExtractionSourceHTML(source){
  return source?.extractionId?`<button type="button" class="button ghost small" data-ke-receipt="${escapeHTML(source.extractionId)}">读取提炼来源凭证</button><div data-ke-source-result></div>`:'';
}
function knowledgeExtractionPanelHTML(view){
  const draft=view.extraction||(view.extraction={runId:'',runs:[],run:null,artifactId:'',receipt:null});
  return `<details class="cd-section" data-knowledge-extraction><summary>从运行产物生成候选</summary><p>选择成功运行中的知识建议产物，核对固定来源后生成待审核记忆和技能。批准、检索和团队版本发布仍需单独操作。</p><button type="button" class="button ghost small" data-ke-runs>读取成功运行</button><label class="cd-field">来源运行<select data-ke-run><option value="">选择运行</option>${draft.runs.map(run=>`<option value="${escapeHTML(run.id)}" ${draft.runId===run.id?'selected':''}>${escapeHTML(run.name||run.workflowId||run.id)} · ${escapeHTML(run.id)}</option>`).join('')}</select></label><button type="button" class="button small" data-ke-load>读取固定产物</button><div data-ke-review></div><div data-ke-result role="status" aria-live="polite">${draft.receipt?`<p>已保留提炼凭证；候选仍需各自审核。</p><pre class="cd-json">${escapeHTML(JSON.stringify(draft.receipt,null,2))}</pre>`:''}</div></details>`;
}
function bindKnowledgeExtraction(host,projectId,view){
  const panel=host.querySelector('[data-knowledge-extraction]');if(!panel)return;
  const draft=view.extraction,base=`/api/projects/${encodeURIComponent(projectId)}/knowledge-extractions`,runs=panel.querySelector('[data-ke-run]'),review=panel.querySelector('[data-ke-review]'),result=panel.querySelector('[data-ke-result]');
  const active=()=>panel.isConnected&&host.dataset.tiProject===projectId&&host.querySelector('[data-knowledge-extraction]')===panel;
  const setBusy=value=>{draft.busy=value;panel.querySelectorAll('button,select').forEach(control=>{if(value){if(!('keDisabled' in control.dataset))control.dataset.keDisabled=String(control.disabled);control.disabled=true;}else if('keDisabled' in control.dataset){control.disabled=control.dataset.keDisabled==='true';delete control.dataset.keDisabled;}});};
  const finish=()=>{draft.busy=false;if(active())setBusy(false);else{const live=document.querySelector(`[data-ti-project="${CSS.escape(projectId)}"]`);if(live)bindKnowledgeExtraction(live,projectId,view);}};
  const fail=error=>{if(active())result.innerHTML=deliveryErrorHTML(error.message||'提炼失败，请核对来源后重试。');};
  function renderReview(){
    const run=draft.run;if(!run){review.replaceChildren();return;}
    const artifacts=(run.artifacts||[]).filter(item=>item.mediaType==='application/json'&&/(^|\/)knowledge-candidates\.json$/.test(item.path));
    if(!artifacts.some(item=>item.id===draft.artifactId))draft.artifactId=artifacts[0]?.id||'';
    const pins=run.executionContext?.workspaceBaseSHAs||{};
    review.innerHTML=`<p>固定运行 <code>${escapeHTML(run.id)}</code></p><p>仓库与提交</p><pre class="cd-json">${escapeHTML(JSON.stringify(pins,null,2))}</pre><label class="cd-field">知识建议产物<select data-ke-artifact>${artifacts.map(item=>`<option value="${escapeHTML(item.id)}" ${draft.artifactId===item.id?'selected':''}>${escapeHTML(item.name||item.path)} · ${escapeHTML(item.nodeId)} · 第 ${item.attempt} 次</option>`).join('')}</select></label><p class="cd-note">仅支持单仓库、固定提交、成功 Agent 节点的只读提炼结果。受限分发来源暂不支持；模型声明的事实与许可均需人工核对。</p><div data-ke-artifact-detail></div><button type="button" class="button primary small" data-ke-import ${!artifacts.length||Object.keys(pins).length!==1?'disabled':''}>生成隔离候选</button>`;
    const select=review.querySelector('[data-ke-artifact]'),detail=review.querySelector('[data-ke-artifact-detail]');
    const show=()=>{draft.artifactId=select.value;const artifact=artifacts.find(item=>item.id===select.value);detail.innerHTML=artifact?`<p>${artifact.size} 字节 · <code>${escapeHTML(artifact.sha256)}</code></p><details><summary>原始建议内容</summary><pre class="cd-json">${escapeHTML(artifact.content||'正文由服务端按固定产物读取并验证。')}</pre></details>`:deliveryEmpty('该运行没有 knowledge-candidates.json 产物。');};
    select.onchange=show;show();
    review.querySelector('[data-ke-import]').onclick=async()=>{
      if(draft.busy||view.busy||view.loading)return;
      const artifact=artifacts.find(item=>item.id===draft.artifactId),entries=Object.entries(pins);if(!artifact||entries.length!==1)return;
      const [repositoryId,expectedCommit]=entries[0],input={runId:run.id,nodeId:artifact.nodeId,artifactId:artifact.id,expectedArtifactSHA256:artifact.sha256,repositoryId,expectedCommit};
      setBusy(true);
      try{
        if(!(await afConfirm({title:'生成隔离候选',message:'将此固定产物转成待审核记忆和技能。模型建议不会自动获准使用；重复导入同一来源会返回原凭证。',confirmText:'生成候选'})))return;
        if(!active()||draft.run!==run||draft.artifactId!==artifact.id)return;
        const response=await api(base,{method:'POST',body:JSON.stringify(input)});
        if(!active())return;
        if(!response?.receipt?.id)throw new Error('服务端未返回提炼凭证，请保留来源并重新核对。');
        draft.receipt=response.receipt;
        await loadTeamIntelligence(projectId);
        toast(response.replayed?'已读取原提炼凭证；未重复创建候选':'已生成隔离候选，请逐项审核');
      }catch(error){fail(error)}finally{finish();if(active())renderReview();}
    };
  }
  runs.onchange=()=>{draft.runId=runs.value;draft.run=null;draft.artifactId='';renderReview();};
  panel.querySelector('[data-ke-runs]').onclick=async()=>{
    if(draft.busy||view.busy||view.loading)return;setBusy(true);
    try{const items=await api(`/api/runs?projectId=${encodeURIComponent(projectId)}`,{silent:true});if(!active())return;draft.runs=(Array.isArray(items)?items:[]).filter(run=>run.status==='succeeded');runs.innerHTML='<option value="">选择运行</option>'+draft.runs.map(run=>`<option value="${escapeHTML(run.id)}" ${draft.runId===run.id?'selected':''}>${escapeHTML(run.name||run.workflowId||run.id)} · ${escapeHTML(run.id)}</option>`).join('');if(!draft.runs.length)result.textContent='暂无成功运行。先通过现有 SOP 生成知识建议产物。';}catch(error){fail(error)}finally{finish()}
  };
  panel.querySelector('[data-ke-load]').onclick=async()=>{
    if(draft.busy||!runs.value||view.busy||view.loading)return;const id=runs.value;setBusy(true);draft.run=null;renderReview();
    try{const run=await api(`/api/runs/${encodeURIComponent(id)}`,{silent:true});if(!active()||draft.runId!==id)return;if(run.status!=='succeeded'||run.executionContext?.projectId!==projectId)throw new Error('来源运行已变化或不属于当前项目，请重新读取。');draft.run=run;renderReview();}catch(error){fail(error)}finally{finish()}
  };
  renderReview();setBusy(draft.busy===true);
  if(!host.dataset.keBound){host.dataset.keBound='true';host.addEventListener('click',async event=>{
    const button=event.target.closest('[data-ke-receipt]');if(!button||!host.contains(button))return;
    const output=button.nextElementSibling;button.disabled=true;
    try{const receipt=await api(`${base}/${encodeURIComponent(button.dataset.keReceipt)}`,{silent:true});if(button.isConnected)output.innerHTML=`<pre class="cd-json">${escapeHTML(JSON.stringify(receipt,null,2))}</pre>`;}catch(error){if(button.isConnected)output.innerHTML=deliveryErrorHTML(error.message)}finally{if(button.isConnected)button.disabled=false}
  });}
}
