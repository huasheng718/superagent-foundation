// Git import previews are drafts. Only an explicit approval publishes a release.
function teamGitImportPanelHTML(view,manage,projectId){
  if(!manage)return '';
  const draft=view.gitImport||(view.gitImport={root:'.',selection:'{\n  "agents": [],\n  "rules": ["rules/review.md"],\n  "skills": [],\n  "mcpServers": []\n}',name:'',repositoryId:'',commit:'',preview:null,error:'',busy:false});
  const repositories=(state.data.repositories||[]).filter(item=>item.projectId===projectId);
  return `<details class="cd-section" data-team-git><summary>从 TeamAI Git 导入</summary>
    <p>从本项目已登记仓库的固定提交读取配置。先检查原始内容及兼容性，再明确批准为团队版本。</p>
    <div class="cd-form-grid"><label class="cd-field">来源仓库<select data-tg-field="repositoryId"><option value="">选择本项目仓库</option>${repositories.map(item=>`<option value="${escapeHTML(item.id)}" ${draft.repositoryId===item.id?'selected':''}>${escapeHTML(item.name||item.id)}</option>`).join('')}</select></label>
    <label class="cd-field">完整提交 SHA<input data-tg-field="commit" value="${escapeHTML(draft.commit)}" placeholder="40 位提交 SHA" spellcheck="false"></label>
    <label class="cd-field">TeamAI 根目录<input data-tg-field="root" value="${escapeHTML(draft.root)}" placeholder=". 或 .teamai"></label>
    <label class="cd-field">新版本名称<input data-tg-field="name" value="${escapeHTML(draft.name)}" maxlength="120"></label></div>
    <label class="cd-field">选择资源与角色绑定（JSON）<textarea data-tg-field="selection" rows="10" spellcheck="false">${escapeHTML(draft.selection)}</textarea></label>
    <p class="cd-note">资源路径相对于上述根目录。角色绑定需明确列出 rules、skills、mcpServers；上游工具白名单、分发角色等语义不自动映射。</p>
    <details><summary>配置选择示例</summary><pre class="cd-json">${escapeHTML(JSON.stringify({agents:['agents/reviewer.yaml'],rules:['rules/review.md'],skills:['skills/review'],mcpServers:[{path:'mcp/mcp.yaml',name:'formatter'}],bindings:{'agents/reviewer.yaml':{rules:['rules/review.md'],skills:['skills/review'],mcpServers:['mcp/mcp.yaml#formatter']}}},null,2))}</pre></details>
    <div class="cd-actions"><button type="button" class="button small" data-tg-preview ${draft.busy?'disabled':''}>预览固定提交</button><button type="button" class="button primary small" data-tg-publish ${draft.busy||!draft.preview?'disabled':''}>批准并发布团队版本</button></div>
    <div data-tg-result role="status" aria-live="polite">${draft.error?deliveryErrorHTML(draft.error):''}${teamGitPreviewHTML(draft)}</div></details>`;
}

function teamGitPreviewHTML(draft){
  if(!draft.preview)return '';
  return `<p>已读取固定提交；发布基线 v${draft.baseVersion}。请审阅全部配置、资源摘要及未导入语义。</p><code>${escapeHTML(draft.preview.sha256||'')}</code><pre class="cd-json">${escapeHTML(JSON.stringify(draft.preview,null,2))}</pre>`;
}

function bindTeamGitImport(host,projectId,view){
  const panel=host.querySelector('[data-team-git]');if(!panel)return;
  const draft=view.gitImport,previewButton=panel.querySelector('[data-tg-preview]'),publishButton=panel.querySelector('[data-tg-publish]');
  const controls=[...panel.querySelectorAll('[data-tg-field]')];
  const update=()=>{const live=host.querySelector('[data-team-git]');if(!live)return;live.querySelectorAll('[data-tg-field]').forEach(control=>control.disabled=draft.busy);live.querySelector('[data-tg-preview]').disabled=draft.busy||view.busy||view.loading||!view.loaded;live.querySelector('[data-tg-publish]').disabled=draft.busy||view.busy||view.loading||!view.loaded||!draft.preview;live.querySelector('[data-tg-result]').innerHTML=(draft.error?deliveryErrorHTML(draft.error):'')+teamGitPreviewHTML(draft);};
  controls.forEach(control=>control.addEventListener('input',()=>{draft[control.dataset.tgField]=control.value;draft.preview=null;draft.error='';update();}));
  previewButton.addEventListener('click',async()=>{
    if(draft.busy||view.busy||view.loading||!view.loaded)return;
    draft.error='';draft.preview=null;
    let input;
    try{
      if(!draft.repositoryId||!/^[a-f0-9]{40}$/.test(draft.commit.trim()))throw new Error('请选择项目仓库并填写完整小写提交 SHA');
      input={repositoryId:draft.repositoryId,commit:draft.commit.trim(),root:draft.root.trim(),selection:JSON.parse(draft.selection)};
      if(!input.selection||Array.isArray(input.selection)||typeof input.selection!=='object')throw new Error('资源选择须为 JSON 对象');
    }catch(error){draft.error=error.message;update();return;}
    draft.busy=true;update();
    // Retain this observed version through approval; never refetch and rebase a
    // stale publication behind the reviewer's back.
    const baseVersion=Math.max(0,...view.releases.map(item=>item.version||0));
    try{
      const preview=await api(`/api/projects/${encodeURIComponent(projectId)}/team-imports/preview`,{method:'POST',body:JSON.stringify(input)});
      if(!/^[a-f0-9]{64}$/.test(preview.sha256||''))throw new Error('预览缺少可核对的内容摘要');
      draft.preview=preview;draft.input=input;draft.baseVersion=baseVersion;
    }catch(error){draft.error=error.message;}
    finally{draft.busy=false;update();}
  });
  publishButton.addEventListener('click',async()=>{
    if(draft.busy||view.busy||view.loading||!view.loaded||!draft.preview)return;
    if(!draft.name.trim()){draft.error='请填写新版本名称';update();return;}
    const reviewed=draft.preview,input={...draft.input,name:draft.name.trim(),expectedBaseVersion:draft.baseVersion,expectedPreviewSha256:reviewed.sha256,approve:true};
    draft.busy=true;update();
    try{
      if(!(await afConfirm({title:'批准 Git 配置为正式版本',message:'请确认已审阅配置、许可资源及未导入语义。将发布当前固定提交及摘要对应的不可变版本，允许后续任务执行。',confirmText:'批准并发布'})))return;
      if(!panel.isConnected||host.querySelector('[data-team-git]')!==panel||host.dataset.tiProject!==projectId||!intelligenceCanManage(projectId))return;
      await api(`/api/projects/${encodeURIComponent(projectId)}/team-imports/publish`,{method:'POST',body:JSON.stringify(input)});
      draft.preview=null;draft.error='';toast('Git 配置已发布为正式团队版本');
      draft.busy=false;
      await loadTeamIntelligence(projectId);
    }catch(error){draft.error=error.message;}
    finally{draft.busy=false;update();}
  });
  update();
}

function teamGitExportPanelHTML(release){
  const manifest=release.payload?.gitImport;
  if(!manifest||release.state!=='published')return '';
  const files=manifest.files.filter(file=>!['teamai.yaml','LICENSE','NOTICE','LICENSE.md','NOTICE.md'].includes(file.path));
  return `<details class="cd-gate-form" data-tg-export><summary>生成资源替换补丁</summary><p>选择固定版本中的资源并上传替换文件。预览会重新核对原提交及批准内容，补丁需另行审核和应用。</p><label class="cd-field">资源路径<select data-tg-export-path>${files.map(file=>`<option value="${escapeHTML(file.path)}" data-mode="${escapeHTML(file.mode)}">${escapeHTML(file.path)}</option>`).join('')}</select></label><label class="cd-field">替换文件<input type="file" data-tg-export-file></label><label class="cd-field"><span><input type="checkbox" data-tg-export-executable> 保留可执行权限</span></label><button type="button" class="button small" data-tg-export-preview>预览替换补丁</button><div data-tg-export-result role="status" aria-live="polite"></div></details>`;
}

function bindTeamGitExport(host,projectId,release){
  const panel=host.querySelector('[data-tg-export]');if(!panel)return;
  const path=panel.querySelector('[data-tg-export-path]'),upload=panel.querySelector('[data-tg-export-file]'),executable=panel.querySelector('[data-tg-export-executable]'),button=panel.querySelector('[data-tg-export-preview]'),result=panel.querySelector('[data-tg-export-result]');
  const mode=()=>{executable.checked=path.selectedOptions[0]?.dataset.mode==='100755';};path.addEventListener('change',()=>{mode();result.replaceChildren();});upload.addEventListener('change',()=>result.replaceChildren());mode();
  executable.addEventListener('change',()=>result.replaceChildren());
  button.addEventListener('click',async()=>{
    const file=upload.files?.[0];if(!file||!path.value)return toast('请选择资源并上传替换文件','err');
    if(file.size>1048576)return toast('替换文件不能超过 1 MiB','err');
    button.disabled=true;path.disabled=true;upload.disabled=true;executable.disabled=true;result.textContent='正在核对源提交并生成补丁…';
    try{
      const bytes=new Uint8Array(await file.arrayBuffer());let binary='';for(let offset=0;offset<bytes.length;offset+=8192)binary+=String.fromCharCode(...bytes.subarray(offset,offset+8192));
      const changes=[{path:path.value,content:btoa(binary),executable:executable.checked}];
      const preview=await api(`/api/projects/${encodeURIComponent(projectId)}/team-releases/${encodeURIComponent(release.id)}/git-export-preview`,{method:'POST',body:JSON.stringify({expectedReleaseSha256:release.sha256,changes})});
      result.innerHTML=`<p>应用前核对基础提交：<code>${escapeHTML(preview.baseCommit)}</code></p><p>补丁摘要：<code>${escapeHTML(preview.patchSha256)}</code></p><div class="cd-actions"><button type="button" class="button small" data-tg-download>下载待审核补丁</button><button type="button" class="button small" data-tg-proposal-prepare ${!/^[a-f0-9]{64}$/.test(preview.patchSha256||'')?'disabled':''}>保存为配置提案</button></div><details><summary>补丁与拟修改配置</summary><pre class="cd-json">${escapeHTML(preview.patch)}</pre><pre class="cd-json">${escapeHTML(JSON.stringify(preview.proposed,null,2))}</pre></details>`;
      bindTeamProposalPrepare(result,projectId,release,preview,changes);
      result.querySelector('[data-tg-download]').addEventListener('click',()=>{const url=URL.createObjectURL(new Blob([preview.patch],{type:'text/x-patch'}));const link=document.createElement('a');link.href=url;link.download='teamai-change.patch';link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);});
    }catch(error){result.innerHTML=deliveryErrorHTML(error.message);}finally{button.disabled=false;path.disabled=false;upload.disabled=false;executable.disabled=false;}
  });
}

function teamGitUpdatePanelHTML(release,view){
  if(!release.payload?.gitImport||release.state!=='published')return '';
  view.gitUpdates??={};
  const draft=view.gitUpdates[release.id]??={name:release.name||'',busy:false,error:'',update:null,reviewed:false};
  return `<details class="cd-gate-form" data-tg-update="${escapeHTML(release.id)}"><summary>检查登记分支更新</summary><p>读取项目登记的默认分支，沿用当前版本的资源选择。检查后固定具体提交；经审阅批准才发布新版本。</p><label class="cd-field">新版本名称<input data-tg-update-name maxlength="120" value="${escapeHTML(draft.name)}"></label><div class="cd-actions"><button type="button" class="button small" data-tg-update-check>检查更新</button><button type="button" class="button primary small" data-tg-update-publish>批准此更新为新版本</button></div><div data-tg-update-result role="status" aria-live="polite"></div></details>`;
}

function bindTeamGitUpdate(host,projectId,release,view){
  const panel=host.querySelector('[data-tg-update]');if(!panel)return;
  const draft=view.gitUpdates[release.id],check=panel.querySelector('[data-tg-update-check]'),publish=panel.querySelector('[data-tg-update-publish]'),name=panel.querySelector('[data-tg-update-name]'),result=panel.querySelector('[data-tg-update-result]');
  const active=()=>panel.isConnected&&host.querySelector('[data-tg-update]')===panel&&host.closest('[data-ti-project]')?.dataset.tiProject===projectId&&intelligenceCanManage(projectId);
  const render=()=>{
    // Refresh can replace a pending panel. Release the live controls without
    // transferring the discarded request's preview or approval to that panel.
    const live=document.querySelector(`[data-ti-project="${CSS.escape(projectId)}"] [data-tg-update="${CSS.escape(release.id)}"]`);
    if(live){
      const disabled=draft.busy||view.busy||view.loading||!view.loaded;
      live.querySelector('[data-tg-update-name]').disabled=live.querySelector('[data-tg-update-check]').disabled=disabled;
      live.querySelector('[data-tg-update-publish]').disabled=disabled||!draft.reviewed||draft.update?.status!=='updated';
      const detail=live.closest('.ti-row')?.querySelector('[data-ti-release-detail]');if(detail)detail.disabled=disabled;
    }
    if(!active())return;
    const update=draft.update;
    result.innerHTML=(draft.error?deliveryErrorHTML(draft.error):'')+(update?`<p>${update.status==='unchanged'?'登记分支没有新提交。':`检查时的发布基线 v${update.expectedBaseVersion}，请审阅下列完整配置与资源。`}</p><p>来源引用：<code>${escapeHTML(update.ref)}</code></p><p>原提交：<code>${escapeHTML(release.payload.gitImport.commit)}</code><br>检查到的提交：<code>${escapeHTML(update.commit)}</code></p>${update.status==='updated'?`<p class="cd-note">发布前会再次检查登记分支；分支变化后须重新检查并审阅。Git 与版本数据库之间没有跨系统原子事务。</p><details open><summary>完整拟发布内容、资源变化与兼容性</summary><pre class="cd-json">${escapeHTML(JSON.stringify({preview:update.preview,changes:update.changes},null,2))}</pre></details>`:''}`:'');
  };
  name.addEventListener('input',()=>{draft.name=name.value;});
  check.addEventListener('click',async()=>{
    if(check.disabled||draft.busy||!active())return;
    draft.busy=true;draft.error='';draft.reviewed=false;draft.update=null;render();
    try{
      const update=await api(`/api/projects/${encodeURIComponent(projectId)}/team-releases/${encodeURIComponent(release.id)}/git-update-preview`,{method:'POST',body:JSON.stringify({expectedReleaseSha256:release.sha256})});
      if(!active())return;
      if(!['updated','unchanged'].includes(update.status)||update.baseReleaseId!==release.id||update.baseReleaseSha256!==release.sha256||!/^refs\/heads\/.+/.test(update.ref||'')||!/^[a-f0-9]{40}$/.test(update.commit||'')||(update.status==='updated'&&!/^[a-f0-9]{64}$/.test(update.preview?.sha256||'')))throw new Error('更新预览缺少可核对的来源或内容摘要');
      draft.update=update;draft.reviewed=update.status==='updated';
    }catch(error){if(active())draft.error=error.message;}
    finally{draft.busy=false;render();}
  });
  publish.addEventListener('click',async()=>{
    if(publish.disabled||draft.busy||!draft.reviewed||!active())return;
    if(!draft.name.trim()){draft.error='请填写新版本名称';render();return;}
    const update=draft.update,manifest=update.preview.payload.gitImport;
    const input={repositoryId:manifest.repositoryId,commit:update.commit,root:manifest.root,selection:manifest.selection,name:draft.name.trim(),expectedBaseVersion:update.expectedBaseVersion,expectedPreviewSha256:update.preview.sha256,approve:true,updateFrom:{releaseId:release.id,releaseSha256:release.sha256,ref:update.ref}};
    draft.busy=true;render();
    try{
      if(!(await afConfirm({title:'批准登记分支的固定更新',message:`请确认已审阅完整配置、许可资源及兼容性。将批准固定提交 ${update.commit}，创建新的不可变团队版本。`,confirmText:'批准并发布'}))||!active())return;
      await api(`/api/projects/${encodeURIComponent(projectId)}/team-imports/publish`,{method:'POST',body:JSON.stringify(input)});
      draft.update=null;draft.reviewed=false;draft.error='';
      if(active()){toast('分支更新已发布为固定团队版本');await loadTeamIntelligence(projectId);}
    }catch(error){draft.error=error.message+(error.status===409?'；请重新检查更新并审阅，原版本与输入已保留。':'');draft.reviewed=false;}
    finally{draft.busy=false;render();}
  });
  render();
}
