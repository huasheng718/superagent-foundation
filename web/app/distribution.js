// Distribution governs explicit platform operations. Inspection and execution
// retain their separate authorities; these controls do not promise DRM.
function distributionEffective(item){
  const result={copy:'allow',edit:'allow',export:'allow'};
  const apply=policy=>{if(policy)for(const key of Object.keys(result))if(policy[key]!=='allow')result[key]='deny'};
  apply(item?.distribution);for(const origin of item?.distributionOrigins||[])apply(origin.policy);
  return result;
}
function distributionCanManage(item){
  if(!state.authRequired||isPlatformAdmin())return true;
  const uid=Number(state.currentUser?.id||0);if(!uid)return false;
  if(item?.scope==='personal')return Number(item.ownerUserId)===uid;
  return !!item?.projectId&&(state.data?.projects||[]).some(project=>project.id===item.projectId&&Number(project.ownerId)===uid);
}
function mountDistributionAction(host,kind,item){
  if(!host)return;
  host.querySelector('[data-distribution-open]')?.remove();
  if(!item?.id||!item.sourceRevision||!['agents','rules','skills','mcp-servers'].includes(kind))return;
  const button=document.createElement('button');button.type='button';button.className='button ghost small';button.dataset.distributionOpen=kind;button.textContent='分发权限';
  button.onclick=()=>openDistributionDialog(kind,item);host.appendChild(button);
}
function openDistributionDialog(kind,item){
  document.getElementById('distributionDialog')?.remove();
  const originalFocus=document.activeElement,overlay=document.createElement('div');overlay.id='distributionDialog';overlay.className='modal open distribution-modal';
  let observed=JSON.parse(JSON.stringify(item)),base=observed.sourceRevision,pending=null,busy=false,conflict=false;
  const labels={copy:'平台复制',edit:'内容编辑',export:'文件／包／补丁导出'};
  const canManage=distributionCanManage(observed);
  const initial=distributionEffective(observed);
  overlay.innerHTML=`<section class="modal-card distribution-card" role="dialog" aria-modal="true" aria-labelledby="distributionTitle"><button type="button" class="modal-close" data-distribution-close aria-label="关闭">×</button><h2 id="distributionTitle">${escapeHTML(item.name||item.id)} · 分发权限</h2><p class="modal-hint">控制平台提供的复制、编辑和显式导出。已授权查看的内容仍可保存；执行权限单独管理。</p><div data-distribution-current></div><details><summary>继承来源与限制</summary><div data-distribution-origins></div></details><form data-distribution-form><div class="distribution-fields">${Object.entries(labels).map(([key,label])=>`<label>${label}<select class="select" name="${key}" ${canManage?'':'disabled'}><option value="allow" ${initial[key]==='allow'?'selected':''}>允许</option><option value="deny" ${initial[key]==='deny'?'selected':''}>禁止</option></select></label>`).join('')}</div>${canManage?'<label>调整理由<textarea name="reason" rows="3" required maxlength="1000" placeholder="记录此次权限决策的理由"></textarea></label>':'<p class="modal-hint">仅来源所属项目 Owner、个人资源本人或平台管理员可以调整。</p>'}<p class="distribution-status" role="status" aria-live="polite" data-distribution-status></p><div data-distribution-compare></div><div class="modal-actions"><button type="button" class="button ghost" data-distribution-close>关闭</button>${canManage?'<button type="submit" class="button primary" data-distribution-save>保存权限</button>':''}</div></form></section>`;
  document.body.appendChild(overlay);
  const form=overlay.querySelector('form'),status=overlay.querySelector('[data-distribution-status]'),save=overlay.querySelector('[data-distribution-save]');
  const active=()=>overlay.isConnected&&document.getElementById('distributionDialog')===overlay;
  const close=()=>{overlay.remove();if(originalFocus?.isConnected)originalFocus.focus()};
  overlay.querySelectorAll('[data-distribution-close]').forEach(button=>button.onclick=close);
  overlay.onkeydown=event=>{
    if(event.key==='Escape'){event.stopPropagation();close();return}
    if(event.key==='Tab'){
      const controls=[...overlay.querySelectorAll('button,input,select,textarea,summary')].filter(el=>!el.disabled&&!el.hidden);
      const first=controls[0],last=controls.at(-1);
      if(event.shiftKey&&document.activeElement===first){event.preventDefault();last?.focus()}
      else if(!event.shiftKey&&document.activeElement===last){event.preventDefault();first?.focus()}
    }
  };
  function renderObserved(){
    const effective=distributionEffective(observed),floor=distributionEffective({distributionOrigins:observed.distributionOrigins});
    overlay.querySelector('[data-distribution-current]').innerHTML=`<dl class="distribution-values">${Object.entries(labels).map(([key,label])=>`<dt>${label}</dt><dd>${effective[key]==='allow'?'允许':'禁止'}${floor[key]==='deny'?'（继承限制）':''}</dd>`).join('')}</dl><p class="modal-hint">${observed.distribution?'已明确设置本层权限':'未设置本层权限，沿用现有行为'}。基线 <code>${escapeHTML(base)}</code></p>`;
    overlay.querySelector('[data-distribution-origins]').innerHTML=(observed.distributionOrigins||[]).length?`<ul>${observed.distributionOrigins.map(origin=>`<li><b>${escapeHTML(origin.kind)} / ${escapeHTML(origin.id)}</b><small>${escapeHTML(origin.projectId||'平台')} · ${escapeHTML(origin.sourceRevision)}</small><span>${Object.entries(labels).map(([key,label])=>`${label}：${origin.policy?.[key]==='allow'?'允许':'禁止'}`).join('；')}</span></li>`).join('')}</ul>`:'<p class="modal-hint">未记录应用内派生来源。此信息不构成商业许可证明。</p>';
    for(const key of Object.keys(labels))form.elements[key].querySelector('[value="allow"]').disabled=floor[key]==='deny';
  }
  renderObserved();overlay.querySelector('button')?.focus();
  form.onsubmit=async event=>{
    event.preventDefault();if(!canManage||busy||conflict)return;
    const policy=Object.fromEntries(Object.keys(labels).map(key=>[key,form.elements[key].value]));
    const floor=distributionEffective({distributionOrigins:observed.distributionOrigins});
    if(Object.keys(labels).some(key=>floor[key]==='deny'&&policy[key]!=='deny')){status.textContent='继承限制不能放宽，请将对应选项设为禁止。';return}
    const reason=form.elements.reason.value.trim();if(!reason){status.textContent='请填写调整理由。';return}
    busy=true;save.disabled=true;status.textContent='正在保存权限…';
    try{
      const result=await api(`/api/${kind}/${encodeURIComponent(item.id)}/distribution`,{method:'PUT',headers:sourceEditHeaders(base),body:JSON.stringify({policy,reason})});
      if(!active())return;
      const key=kind==='mcp-servers'?'mcpServers':kind;
      const list=state.data?.[key]||[],at=list.findIndex(value=>value.id===item.id);
      if(at>=0&&list[at].sourceRevision===base)list[at]={...list[at],distribution:result.distribution,distributionOrigins:result.distributionOrigins,sourceRevision:result.sourceRevision};
      observed={...observed,...result};base=result.sourceRevision;renderObserved();
      status.textContent='权限已保存。已有正文草稿仍保留原基线；继续编辑前请关闭并重新打开资源核对。';
    }catch(error){
      if(!active())return;
      status.textContent=error.message;
      if(error.status===409||error.code==='source_edit_conflict'){
        conflict=true;status.textContent='资源已变化，权限草稿和理由已保留。请读取并核对最新权限后，明确采用新基线。';
        const area=overlay.querySelector('[data-distribution-compare]');area.innerHTML='<button type="button" class="button ghost small" data-distribution-load>读取最新权限</button><div data-distribution-latest></div>';
        area.querySelector('button').onclick=async()=>{
          const button=area.querySelector('button');button.disabled=true;
          try{
            const list=await api(`/api/${kind}`,{silent:true});if(!active())return;
            pending=(Array.isArray(list)?list:[]).find(value=>value.id===item.id);if(!pending)throw new Error('来源已不存在或不可读取');
            const effective=distributionEffective(pending),host=area.querySelector('[data-distribution-latest]');
            host.innerHTML=`<p>${Object.entries(labels).map(([key,label])=>`${label}：${effective[key]==='allow'?'允许':'禁止'}`).join('；')}<br>新基线 <code>${escapeHTML(pending.sourceRevision)}</code></p><button type="button" class="button ghost small" data-distribution-adopt>已核对，采用新基线</button>`;
            host.querySelector('button').onclick=()=>{observed=JSON.parse(JSON.stringify(pending));base=observed.sourceRevision;renderObserved();conflict=false;save.disabled=false;area.innerHTML='';status.textContent='已采用核对后的基线，草稿未自动重放。请复核选项并再次保存。'};
          }catch(error){if(active())status.textContent=error.message}finally{if(active())button.disabled=false}
        };
      }else save.disabled=false;
    }finally{busy=false}
  };
}
