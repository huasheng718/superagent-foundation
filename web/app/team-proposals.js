// Configuration proposals have their own human review. They do not stand in
// for execution gates and never publish a formal release or merge a PR.
function teamProposalView(view){
  return view.teamProposals??={items:[],detail:null,selected:'',busy:false,error:'',stale:false,reason:'',loaded:false};
}
function teamProposalsPanelHTML(view,manage){
  if(!manage)return '';
  teamProposalView(view);
  return `<section class="cd-section" data-team-proposals><div class="cd-section-heading"><h4>配置变更提案</h4><button type="button" class="button ghost small" data-tp-refresh>读取提案</button></div><p>先在 Git 版本详情预览资源替换补丁，再保存提案。审阅批准后，由管理员单独创建草稿 PR。</p><div data-tp-list></div><div data-tp-detail aria-live="polite"></div></section>`;
}
function teamProposalStateLabel(value){return ({pending_review:'待人工审核',approved:'已批准',rejected:'已拒绝',cancelled:'已取消'})[value]||value||'未知';}
function teamProposalDetailHTML(v,draft){
  if(!v)return draft.error?deliveryErrorHTML(draft.error):'';
  const publication=v.publication,uncertain=publication?.state==='uncertain'||publication?.prCreateAttempted&&!publication?.prURL;
  return `${draft.error?deliveryErrorHTML(draft.error):''}<article class="ti-row"><div class="ti-row-head"><h5>${escapeHTML(teamProposalStateLabel(v.state))}</h5><span>修订 ${v.revision}</span></div><p>目标仓库 ${escapeHTML(v.providerProject||v.repositoryId)}<br><code>${escapeHTML(v.targetRef)}</code></p><p>基础提交 <code>${escapeHTML(v.baseCommit)}</code><br>提案提交 <code>${escapeHTML(v.resultSHA)}</code></p><p>审阅摘要 <code>${escapeHTML(v.sha256)}</code></p><p class="cd-note">此提案只验证配置与 Git 资源完整性，未通过执行任务的 Fast／Slow 门禁。目标分支检查与创建 PR 之间没有原子事务。</p><details open><summary>修改的文件与模式</summary>${(v.files||[]).map(file=>`<p><code>${escapeHTML(file.path)}</code><br>原始：${escapeHTML(file.before?.mode)}，${file.before?.size??0} 字节，<code>${escapeHTML(file.before?.sha256)}</code><br>替换：${escapeHTML(file.after?.mode)}，${file.after?.size??0} 字节，<code>${escapeHTML(file.after?.sha256)}</code></p>`).join('')}</details><details open><summary>精确补丁</summary><pre class="cd-json">${escapeHTML(v.patch||'')}</pre></details><details><summary>验证与审核记录</summary><pre class="cd-json">${escapeHTML(JSON.stringify({validation:v.validation,decisions:v.decisions},null,2))}</pre></details>${publication?`<details open><summary>草稿 PR 交付状态</summary><pre class="cd-json">${escapeHTML(JSON.stringify(publication,null,2))}</pre>${uncertain?'<p class="cd-note">创建结果尚未确认。核对只查找原 PR，不会重发创建请求。</p>':''}</details>`:''}<label class="cd-field">审核或取消依据<textarea data-tp-reason rows="3" maxlength="2000">${escapeHTML(draft.reason)}</textarea></label><div class="cd-actions">${v.state==='pending_review'?'<button class="button small" type="button" data-tp-action="approved">批准此提案</button><button class="button danger-outline small" type="button" data-tp-action="rejected">拒绝提案</button>':''}${v.state==='approved'&&isPlatformAdmin()&&publication?.state!=='pr_ready'?`<button class="button primary small" type="button" data-tp-action="${publication?.prCreateAttempted?'reconcile':'publish'}">${publication?.prCreateAttempted?'核对原草稿 PR':'创建草稿 PR'}</button>`:''}${['pending_review','approved'].includes(v.state)?'<button class="button ghost small" type="button" data-tp-action="cancel">取消后续交付</button>':''}<button class="button ghost small" type="button" data-tp-read>重新读取此修订</button></div></article>`;
}
function bindTeamProposals(host,projectId,view){
  const panel=host.querySelector('[data-team-proposals]');if(!panel)return;
  const draft=teamProposalView(view),base=`/api/projects/${encodeURIComponent(projectId)}/team-git-proposals`;
  const active=()=>panel.isConnected&&host.querySelector('[data-team-proposals]')===panel&&host.dataset.tiProject===projectId&&intelligenceCanManage(projectId);
  const render=()=>{
    if(!active())return;
    panel.querySelector('[data-tp-refresh]').disabled=draft.busy||view.busy||view.loading;
    panel.querySelector('[data-tp-list]').innerHTML=draft.items.map(item=>`<button type="button" class="button ghost small" data-tp-id="${escapeHTML(item.id)}" ${draft.busy?'disabled':''}>${escapeHTML(item.resultSHA?.slice(0,12)||item.id.slice(0,12))} · ${escapeHTML(teamProposalStateLabel(item.state))}</button>`).join('')||(draft.loaded?deliveryEmpty('暂无配置提案。先预览 Git 版本的资源替换补丁。'):'');
    const output=panel.querySelector('[data-tp-detail]');output.innerHTML=teamProposalDetailHTML(draft.detail,draft);
    output.querySelector('[data-tp-reason]')?.addEventListener('input',event=>{draft.reason=event.currentTarget.value;});
    output.querySelectorAll('[data-tp-action]').forEach(button=>{button.disabled=draft.busy||draft.stale||view.busy||view.loading;button.addEventListener('click',()=>decide(button.dataset.tpAction));});
    output.querySelector('[data-tp-read]')?.addEventListener('click',()=>read(draft.selected));
    if(draft.busy)output.querySelectorAll('button,textarea').forEach(control=>control.disabled=true);
    panel.querySelectorAll('[data-tp-id]').forEach(button=>button.addEventListener('click',()=>read(button.dataset.tpId)));
  };
  const run=async operation=>{
    if(!active()||draft.busy||view.busy||view.loading)return;
    draft.busy=true;draft.error='';render();
    try{await operation();}catch(error){if(active()){draft.error=(error.message||'操作失败')+'；请重新读取并复核后再操作。';draft.stale=true;}}
    finally{draft.busy=false;if(active())render();else{const live=document.querySelector(`[data-ti-project="${CSS.escape(projectId)}"]`);if(live)bindTeamProposals(live,projectId,view);}}
  };
  const read=async id=>run(async()=>{
    const value=await api(`${base}/${encodeURIComponent(id)}`,{silent:true});
    if(!active())return;
    if(value.id!==id||value.projectId!==projectId||!Number.isInteger(value.revision)||!/^[a-f0-9]{64}$/.test(value.sha256||''))throw new Error('提案缺少可核对的项目、修订或摘要');
    draft.detail=value;draft.selected=id;draft.stale=false;
  });
  const decide=async action=>run(async()=>{
    const value=draft.detail;if(!value||draft.stale)return;
    const reviewed={id:value.id,sha256:value.sha256,revision:value.revision,baseCommit:value.baseCommit};
    const reason=draft.reason.trim(),remote=['publish','reconcile'].includes(action);
    if(!remote&&!reason){toast('请填写审核或取消依据','err');return;}
    if(remote&&!isPlatformAdmin())return;
    const title=({approved:'批准配置提案',rejected:'拒绝配置提案',cancel:'取消后续交付',publish:'创建配置草稿 PR',reconcile:'核对原草稿 PR'})[action];
    if(!(await afConfirm({title,message:remote?'将使用已批准的精确提交。仅创建或查找草稿 PR，合入需在代码托管平台人工处理。':'决定绑定当前修订和内容摘要；保存后不会创建 PR。',confirmText:title})))return;
    if(!active()||draft.detail!==value||!intelligenceCanManage(projectId)||remote&&!isPlatformAdmin())return;
    const input={expectedRevision:reviewed.revision,expectedProposalSha256:reviewed.sha256};
    if(remote)input.expectedBaseCommit=reviewed.baseCommit;else input.reason=reason;
    let endpoint=action;if(['approved','rejected'].includes(action)){endpoint='decision';input.decision=action;}
    await api(`${base}/${encodeURIComponent(reviewed.id)}/${endpoint}`,{method:'POST',body:JSON.stringify(input)});
    if(!active())return;
    // Readback happens only after a successful explicit command. A conflict
    // never fetches a new base and retries a write behind the reviewer's back.
    const updated=await api(`${base}/${encodeURIComponent(reviewed.id)}`,{silent:true});
    if(!active())return;
    draft.detail=updated;draft.stale=false;
    draft.items=draft.items.map(item=>item.id===updated.id?updated:item);
    toast(remote?'草稿 PR 交付状态已更新':'提案决定已保存');
  });
  panel.querySelector('[data-tp-refresh]').onclick=()=>run(async()=>{
    const items=await api(base,{silent:true});if(!active())return;
    draft.items=Array.isArray(items)?items:items.items||[];draft.loaded=true;
    // A list refresh does not approve a newly observed revision.
    draft.stale=true;
  });
  render();
}

function bindTeamProposalPrepare(result,projectId,release,preview,changes){
  const button=result.querySelector('[data-tg-proposal-prepare]');if(!button)return;
  button.addEventListener('click',async()=>{
    const host=result.closest('[data-ti-project]'),panel=result.closest('[data-tg-export]');
    const active=()=>button.isConnected&&host?.dataset.tiProject===projectId&&intelligenceCanManage(projectId)&&panel?.querySelector('[data-tg-proposal-prepare]')===button;
    if(button.disabled||!active())return;
    button.disabled=true;
    try{
      if(!(await afConfirm({title:'保存配置变更提案',message:'保存当前补丁和精确结果提交，供下一步人工审阅。此操作不会推送分支或创建 PR。',confirmText:'保存提案'}))||!active())return;
      const proposal=await api(`/api/projects/${encodeURIComponent(projectId)}/team-git-proposals`,{method:'POST',body:JSON.stringify({releaseId:release.id,expectedReleaseSha256:release.sha256,expectedBaseCommit:preview.baseCommit,expectedPatchSha256:preview.patchSha256,changes})});
      if(!active())return;
      const view=intelligenceView(projectId),draft=teamProposalView(view);
      draft.items=[proposal,...draft.items.filter(item=>item.id!==proposal.id)];draft.detail=proposal;draft.selected=proposal.id;draft.loaded=true;draft.stale=false;draft.error='';
      bindTeamProposals(host,projectId,view);toast('配置提案已保存，请审阅后单独批准');
    }catch(error){if(active())toast(error.message||'提案保存失败','err');}
    finally{if(active())button.disabled=false;}
  });
}
