// Controlled delivery shares the existing Run/Engine and administrator boundary.
// Every state shown below comes from the controller APIs; no optimistic PR state.
const controlledDeliveryViews = new Map();
function deliveryView(runId) {
  if (!controlledDeliveryViews.has(runId)) controlledDeliveryViews.set(runId, {data:null,error:'',loading:false,busy:false,selected:new Set(),integrationId:'',draft:{},gateRun:null});
  return controlledDeliveryViews.get(runId);
}
function deliveryReceipts(task) { return task.contributions || task.acceptedContributions || []; }
function deliveryTaskId(task) { return task.taskId || task.id || ''; }
function deliveryStateLabel(value) { return ({prepared:'已集成',gating:'等待门禁',ready:'可创建草稿 PR',publishing:'正在发布',pr_ready:'已创建草稿 PR',rejected:'门禁拒绝',passed:'通过',failed:'失败'})[value] || value || '待验证'; }
function deliverySafePR(raw) {
  try { const url = new URL(raw); return url.protocol === 'https:' && !url.username && !url.password ? url.href : ''; } catch { return ''; }
}
function deliveryValidTime(raw) { const time=Date.parse(raw); return Number.isFinite(time)&&new Date(time).getUTCFullYear()>1970; }
function deliveryEligibleTasks(tasks,repositoryId) {
  return (tasks||[]).filter(task => task.status==='success' && deliveryReceipts(task).some(receipt=>receipt.repositoryId===repositoryId&&deliveryValidTime(receipt.acceptedAt)));
}
function deliveryRepoOptions(run) {
  const contexts=run.repositories?.length?run.repositories:run.repository?[run.repository]:[];
  const ids=new Set(contexts.map(repo=>repo.repositoryId||repo.id).filter(Boolean));
  return [...ids].map(id=>({id,name:(state.data.repositories||[]).find(repo=>repo.id===id)?.name||id}));
}
function deliveryErrorHTML(message) { return `<p class="cd-error" role="alert">${escapeHTML(message)}</p>`; }
function deliveryEmpty(message) { return `<p class="cd-empty">${escapeHTML(message)}</p>`; }
function controlledDeliveryPaneHTML(run) {
  const view=deliveryView(run.id),data=view.data,admin=isPlatformAdmin(),repos=deliveryRepoOptions(run);
  const repositoryId=view.draft.repositoryId||repos[0]?.id||'';
  const eligible=deliveryEligibleTasks(data?.tasks,repositoryId);
  const integrations=(data?.integrations||[]).filter(item=>!repositoryId||item.repositoryId===repositoryId);
  const selected=integrations.find(item=>item.id===view.integrationId)||integrations[0];
  if(selected)view.integrationId=selected.id;
  const publication=(data?.publications||[]).find(item=>item.id===selected?.id);
  const gateKinds=['fast','slow','review'];
  const reconciling=publication?.state==='publishing'&&publication?.prCreateAttempted;
  const leaseExpired=publication?.leaseUntil&&deliveryValidTime(publication.leaseUntil)&&Date.parse(publication.leaseUntil)<=Date.now();
  const gates=publication?.gates||[],canPublish=admin&&!view.busy&&(publication?.state==='ready'||(reconciling&&leaseExpired))&&run.status==='succeeded'&&!run.cancelRequested;
  const sourceRuns=(state.data.runs||[]).filter(item=>item.id!==run.id&&item.status==='succeeded'&&(!item.executionContext?.projectId||item.executionContext.projectId===run.executionContext?.projectId));
  const gateNodes=(view.gateRun?.workflowSnapshot?.nodes||[]).filter(node=>view.gateRun.nodes?.[node.id]?.status==='succeeded');
  const nodeId=view.draft.gateNodeId||gateNodes[0]?.id||'';
  const artifacts=(view.gateRun?.artifacts||[]).filter(item=>item.nodeId===nodeId);
  const pr=deliverySafePR(publication?.prURL);
  return `<div class="cd-root" data-delivery-run="${escapeHTML(run.id)}" aria-busy="${view.loading||view.busy}">
    <div class="cd-heading"><div><h3>受控交付</h3><p>汇总代码后，对同一个提交完成测试与独立评审，再创建草稿 PR。</p></div><button type="button" class="button ghost small" data-cd-refresh ${view.loading||view.busy?'disabled':''}>${view.loading?'正在刷新…':'刷新'}</button></div>
    ${!admin?deliveryEmpty('受控集成与发布需要平台管理员权限。可在概览中查看运行产物。'):''}
    ${view.error?deliveryErrorHTML(view.error):''}
    ${!data?(view.loading?deliveryEmpty('正在读取代码贡献与交付记录…'):deliveryEmpty('打开此页后读取交付记录。')):''}
    <div data-cd-budget>${budgetSummaryHTML(run)}</div>${intelligenceSummaryHTML(run)}
    <section class="cd-section"><div class="cd-section-heading"><h4>代码贡献</h4><span>仅成功尝试中已接收的代码可参与集成</span></div>
      <label class="cd-field">代码仓库<select data-cd-repository ${view.busy||!repos.length?'disabled':''}>${repos.map(repo=>`<option value="${escapeHTML(repo.id)}" ${repo.id===repositoryId?'selected':''}>${escapeHTML(repo.name)}</option>`).join('')||'<option value="">本次运行未绑定代码仓库</option>'}</select></label>
      <div class="cd-table-scroll"><table class="cd-table"><thead><tr><th scope="col">选择</th><th scope="col">来源任务</th><th scope="col">基础提交</th><th scope="col">结果提交</th></tr></thead><tbody>${eligible.map(task=>{const receipt=deliveryReceipts(task).find(item=>item.repositoryId===repositoryId&&deliveryValidTime(item.acceptedAt));return `<tr><td><input type="checkbox" data-cd-task="${escapeHTML(deliveryTaskId(task))}" aria-label="选择任务 ${escapeHTML(deliveryTaskId(task))}" ${view.selected.has(deliveryTaskId(task))?'checked':''} ${!admin||view.busy?'disabled':''}></td><td><b>${escapeHTML(task.nodeId||deliveryTaskId(task))}</b><small>${escapeHTML(task.runId||'')} · 尝试 ${receipt.attempt||task.attempt||0}</small></td><td><code>${escapeHTML(receipt.baseSHA)}</code></td><td><code>${escapeHTML(receipt.resultSHA)}</code></td></tr>`}).join('')||'<tr><td colspan="4">暂无可集成的代码贡献。先运行启用了独立工作区的任务。</td></tr>'}</tbody></table></div>
      <div class="cd-actions"><span data-cd-selection>${view.selected.size} 个任务已选择</span><button type="button" class="button primary small" data-cd-integrate ${!admin||view.busy||!view.selected.size||!repositoryId||run.status!=='succeeded'||run.cancelRequested?'disabled':''}>集成所选代码</button></div>
    </section>
    <section class="cd-section"><div class="cd-section-heading"><h4>集成与质量门禁</h4><span>门禁证据须对应集成结果 SHA</span></div>
      ${integrations.length?`<label class="cd-field">集成版本<select data-cd-integration ${view.busy?'disabled':''}>${integrations.map(item=>`<option value="${escapeHTML(item.id)}" ${selected?.id===item.id?'selected':''}>${escapeHTML(item.resultSHA.slice(0,12))} · ${escapeHTML(deliveryStateLabel(item.state))}</option>`).join('')}</select></label><dl class="cd-facts"><div><dt>结果提交</dt><dd><code>${escapeHTML(selected.resultSHA)}</code></dd></div><div><dt>上下文</dt><dd><code>${escapeHTML(selected.contextSHA256)}</code></dd></div></dl><div class="cd-gates">${gateKinds.map(kind=>{const gate=gates.find(item=>item.kind===kind);return `<article class="cd-gate"><h5>${({fast:'快速测试',slow:'深入测试',review:'独立评审'})[kind]}</h5><span class="hui-badge" data-tone="${gate?.status==='passed'?'success':gate?'error':'neutral'}">${escapeHTML(deliveryStateLabel(gate?.status))}</span>${gate?`<small>${escapeHTML(gate.gateRunId||'')} / ${escapeHTML(gate.gateNodeId||'')}</small><code>${escapeHTML(gate.evidenceSHA256)}</code>`:'<small>尚未登记验证报告</small>'}</article>`}).join('')}</div>
      ${admin?`<details class="cd-gate-form"><summary>登记已有验证报告</summary><p>选择独立验证运行中的成功节点和报告。服务端会重新核对任务、产物、评审身份及提交版本。</p><div class="cd-form-grid"><label class="cd-field">门禁类型<select data-cd-kind>${gateKinds.map(kind=>`<option value="${kind}" ${view.draft.gateKind===kind?'selected':''}>${({fast:'快速测试',slow:'深入测试',review:'独立评审'})[kind]}</option>`).join('')}</select></label><label class="cd-field">验证运行<select data-cd-gate-run><option value="">选择成功的验证运行</option>${sourceRuns.map(item=>`<option value="${escapeHTML(item.id)}" ${view.draft.gateRunId===item.id?'selected':''}>${escapeHTML(item.workflowSnapshot?.name||item.workflowId||item.id)} · ${escapeHTML(item.id.slice(-12))}</option>`).join('')}</select></label><label class="cd-field">验证节点<select data-cd-gate-node ${gateNodes.length?'':'disabled'}>${gateNodes.map(node=>`<option value="${escapeHTML(node.id)}" ${node.id===nodeId?'selected':''}>${escapeHTML(node.name||node.id)}</option>`).join('')||'<option value="">先选择验证运行</option>'}</select></label><label class="cd-field">报告产物<select data-cd-artifact ${artifacts.length?'':'disabled'}>${artifacts.map(item=>`<option value="${escapeHTML(item.id)}" ${view.draft.artifactId===item.id?'selected':''}>${escapeHTML(item.name||item.path||item.id)} · 尝试 ${item.attempt}</option>`).join('')||'<option value="">该节点没有报告产物</option>'}</select></label></div><button type="button" class="button small" data-cd-record-gate ${view.busy||!view.gateRun||!artifacts.length||publication?.state==='pr_ready'?'disabled':''}>登记并校验报告</button></details>`:''}`:deliveryEmpty('集成后会显示最终提交，以及快速测试、深入测试和独立评审的验证记录。')}
    </section>
    <section class="cd-section"><div class="cd-section-heading"><h4>草稿 PR</h4><span>${escapeHTML(deliveryStateLabel(publication?.state))}</span></div>${publication?`<dl class="cd-facts"><div><dt>目标分支</dt><dd>${escapeHTML(publication.targetBranch||'—')}</dd></div><div><dt>工作分支</dt><dd><code>${escapeHTML(publication.branch||'—')}</code></dd></div><div><dt>发布尝试</dt><dd>${publication.attempt||0}</dd></div></dl>`:deliveryEmpty('完成代码集成后，系统会保留门禁和发布记录。')}${reconciling?'<p class="cd-note">已尝试创建 PR，结果待核对。发布租约到期后可查询同一工作分支；不会重复创建。若尚未发现结果，请稍后刷新，或由管理员核查。</p>':''}<p class="cd-note">创建 PR 后仍需人工审核与合入，业务需求不会自动完成。</p><div class="cd-actions">${pr?`<a class="button primary small" href="${escapeHTML(pr)}" target="_blank" rel="noopener noreferrer">打开草稿 PR${publication.prNumber?' #'+publication.prNumber:''}</a>`:`<button type="button" class="button primary small" data-cd-publish ${canPublish?'':'disabled'}>${view.busy?'正在处理…':reconciling?'核对已有 PR':'创建草稿 PR'}</button>`}${publication&&!reconciling&&publication.state!=='ready'&&publication.state!=='pr_ready'?'<small>三类门禁通过并保持证据有效后才能发布。</small>':''}</div></section>
  </div>`;
}
function renderControlledDelivery(run) {
  const pane=document.querySelector('#runDetail [data-run-pane="delivery"]');
  if(!pane||state.activeRun?.id!==run.id)return;
  pane.innerHTML=controlledDeliveryPaneHTML(run); bindControlledDelivery(run,pane);
}
async function loadControlledDelivery(runId) {
  const view=deliveryView(runId); if(view.loading)return;
  view.loading=true;view.error=''; if(state.activeRun?.id===runId)renderControlledDelivery(state.activeRun);
  try { view.data=await api(`/api/runs/${encodeURIComponent(runId)}/contributions`,{silent:true});
    const valid=new Set((view.data.tasks||[]).map(deliveryTaskId));view.selected=new Set([...view.selected].filter(id=>valid.has(id)));
  } catch(error) { view.error=error.message||'交付记录加载失败，请重试'; }
  finally { view.loading=false;if(state.activeRun?.id===runId)renderControlledDelivery(state.activeRun); }
}
async function deliveryOperation(run,operation) {
  const view=deliveryView(run.id);if(view.busy)return;view.busy=true;view.error='';renderControlledDelivery(run);
  try { await operation();await loadControlledDelivery(run.id); }
  catch(error) { view.error=error.message||'操作失败，请检查条件后重试'; }
  finally { view.busy=false;if(state.activeRun?.id===run.id)renderControlledDelivery(state.activeRun); }
}
function bindControlledDelivery(run,pane) {
  const view=deliveryView(run.id),base=`/api/runs/${encodeURIComponent(run.id)}/contributions`;
  pane.querySelector('[data-cd-refresh]')?.addEventListener('click',()=>loadControlledDelivery(run.id));
  pane.querySelector('[data-cd-repository]')?.addEventListener('change',event=>{view.draft.repositoryId=event.target.value;view.selected.clear();view.integrationId='';renderControlledDelivery(run);});
  pane.querySelector('[data-cd-integration]')?.addEventListener('change',event=>{view.integrationId=event.target.value;renderControlledDelivery(run);});
  pane.querySelectorAll('[data-cd-task]').forEach(box=>box.addEventListener('change',()=>{box.checked?view.selected.add(box.dataset.cdTask):view.selected.delete(box.dataset.cdTask);const button=pane.querySelector('[data-cd-integrate]');if(button)button.disabled=!view.selected.size||run.status!=='succeeded'||run.cancelRequested||view.busy||!isPlatformAdmin();pane.querySelector('[data-cd-selection]').textContent=`${view.selected.size} 个任务已选择`;}));
  pane.querySelector('[data-cd-integrate]')?.addEventListener('click',()=>{const repositoryId=pane.querySelector('[data-cd-repository]').value,taskIds=[...view.selected];deliveryOperation(run,async()=>{const integration=await api(base+'/integrate',{method:'POST',body:JSON.stringify({repositoryId,taskIds})});view.integrationId=integration.id;view.selected.clear();toast('代码已集成，请验证最终提交');});});
  pane.querySelector('[data-cd-kind]')?.addEventListener('change',event=>view.draft.gateKind=event.target.value);
  pane.querySelector('[data-cd-gate-run]')?.addEventListener('change',async event=>{const id=event.target.value;view.draft.gateRunId=id;view.draft.gateNodeId='';view.draft.artifactId='';view.gateRun=null;if(!id){renderControlledDelivery(run);return;}try{const gateRun=await api(`/api/runs/${encodeURIComponent(id)}`,{silent:true});if(view.draft.gateRunId!==id)return;view.gateRun=gateRun;}catch(error){view.error=error.message;}if(state.activeRun?.id===run.id)renderControlledDelivery(run);});
  pane.querySelector('[data-cd-gate-node]')?.addEventListener('change',event=>{view.draft.gateNodeId=event.target.value;view.draft.artifactId='';renderControlledDelivery(run);});
  pane.querySelector('[data-cd-artifact]')?.addEventListener('change',event=>view.draft.artifactId=event.target.value);
  pane.querySelector('[data-cd-record-gate]')?.addEventListener('click',()=>{const input={gateRunId:pane.querySelector('[data-cd-gate-run]').value,nodeId:pane.querySelector('[data-cd-gate-node]').value,kind:pane.querySelector('[data-cd-kind]').value,artifactId:pane.querySelector('[data-cd-artifact]').value};const integrationId=view.integrationId;deliveryOperation(run,async()=>{await api(`${base}/${encodeURIComponent(integrationId)}/gates`,{method:'POST',body:JSON.stringify(input)});toast('验证报告已登记');});});
  pane.querySelector('[data-cd-publish]')?.addEventListener('click',async()=>{const publication=(view.data?.publications||[]).find(item=>item.id===view.integrationId),reconciling=publication?.state==='publishing'&&publication.prCreateAttempted;const label=reconciling?'核对已有 PR':'创建草稿 PR';if(!(await afConfirm({title:label,message:reconciling?'查询同一工作分支上的已有 PR，并重新核对提交和人工审核状态。结果不明确时保留待核对状态。':'将已通过测试与独立评审的固定提交发布为草稿 PR。后续由人工审核与合入。',confirmText:label})))return;deliveryOperation(run,async()=>{await api(`${base}/${encodeURIComponent(view.integrationId)}/publish`,{method:'POST',body:'{}'});toast(reconciling?'已有 PR 已核对':'草稿 PR 已创建');});});
}
function budgetSummaryHTML(run) {
  const policy=run.executionContext?.executionBudget,value=run.executionBudget;
  if(!policy&&!value)return '';
  return `<section class="cd-section"><div class="cd-section-heading"><h4>执行预算</h4><span>${policy?.mode==='strict'?'严格限额':'用量计量'}</span></div><dl class="cd-facts"><div><dt>Token 上限</dt><dd>${escapeHTML(String(policy?.maxTotalTokens||'未配置'))}</dd></div><div><dt>费用上限（USD）</dt><dd>${policy?.maxCostMicrosUSD!=null?escapeHTML(String(policy.maxCostMicrosUSD/1000000)):'未配置'}</dd></div><div><dt>调用上限</dt><dd>${escapeHTML(String(policy?.maxInvocations||'未配置'))}</dd></div><div><dt>无进展上限</dt><dd>${escapeHTML(String(policy?.maxNoProgress||'未配置'))}</dd></div></dl>${value?`<pre class="cd-json">${escapeHTML(JSON.stringify(value,null,2))}</pre>`:''}<p class="cd-note">${policy?.mode==='strict'?'执行器必须支持预授权硬限额，否则拒绝启动。':'按已上报用量停止后续执行；计量模式不能保证外部服务不超额计费。'}</p></section>`;
}

function intelligenceSummaryHTML(run) {
  const context=run.intelligence;if(!context)return '';
  return `<section class="cd-section"><div class="cd-section-heading"><h4>固定执行上下文</h4><span>运行创建时冻结</span></div><dl class="cd-facts"><div><dt>团队版本</dt><dd>${escapeHTML(context.releaseId||'未选择')}</dd></div><div><dt>版本摘要</dt><dd><code>${escapeHTML(context.releaseSha256||'—')}</code></dd></div><div><dt>上下文摘要</dt><dd><code>${escapeHTML(context.sha256||'—')}</code></dd></div><div><dt>已批准记忆</dt><dd>${(context.memories||[]).length} 条</dd></div></dl><details><summary>查看固定上下文</summary><pre class="cd-json">${escapeHTML(JSON.stringify(context,null,2))}</pre></details></section>`;
}
