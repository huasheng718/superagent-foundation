# TeamAI Git 配置导入、更新审阅与补丁预览

平台读取腾讯 `Tencent/teamai-cli` 的实际基础配置格式，研究版本固定为
`2660cfce1c2ff9c8c00c737f6c24ad25f76ba17f`（0.22.0，MIT）。适配器只读
Git 对象，不安装或执行上游 CLI、脚本、hooks，也不修改可编辑 Catalog。

项目负责人或平台管理员在“团队配置与记忆 → 从 TeamAI Git 导入”中选择本项目
登记仓库、完整提交 SHA、配置根目录及资源选择。预览显示固定内容、源文件摘要、
模式和未导入的语义；明确批准后才在现有 `team_releases` 中创建不可变版本。
发布重读相同提交并核对预览摘要，使用预览时的项目版本基线。冲突返回 409，
保留输入，需重新预览和审核。

支持根目录的 `teamai.yaml`，或带 `mode: self` 的 `.teamai/teamai.yaml`。
选择路径相对于该根目录：

```json
{
  "agents": ["agents/reviewer.yaml"],
  "rules": ["rules/review.md"],
  "skills": ["skills/review"],
  "mcpServers": [{"path": "mcp/mcp.yaml", "name": "formatter"}],
  "bindings": {
    "agents/reviewer.yaml": {
      "rules": ["rules/review.md"],
      "skills": ["skills/review"],
      "mcpServers": ["mcp/mcp.yaml#formatter"]
    }
  }
}
```

基础子集包括 Agent 的 name/description/instructions、无作用域 Markdown 规则、
带正确 SKILL.md 的技能包及嵌套资源、简单 stdio/http MCP。角色依赖必须显式绑定。
Agent tools/targets/model、规则作用域、MCP 凭据/环境变量/headers/sse/变量替换等
尚未映射的选中配置会返回 422，不会悄悄丢弃后发布。分发角色和 targets 不代表
LoopForge 成员权限或 Worker 分配；`teamai.lock` 也不作为完整团队配置锁。

生产来源使用无内嵌凭据的 HTTPS 登记地址，读取使用项目所有者现有 Git 凭据。
请求不能传入任意 URL、本机路径或令牌。一次性本机实验可由部署者同时设置
`AGENTFLOW_TEAM_IMPORT_ALLOW_FILE=1` 与绝对路径
`AGENTFLOW_TEAM_IMPORT_FILE_ROOT`，只允许该目录内的 file 仓库。
此选项不应被普通项目用户修改。

每次读取最多 1024 个选中文件、每文件 1 MiB、累计 6 MiB，Git 操作截止时间
90 秒。导入来源、提交、根目录、选择/绑定及每文件的 blob SHA、SHA256、大小和
模式参与正式版本摘要。未知字段、非法路径、符号链接及非法包会明确拒绝。

| 项目下接口 | 请求与行为 |
| --- | --- |
| POST `/team-imports/preview` | `{repositoryId,commit,root,selection}`，只读预览 |
| POST `/team-imports/publish` | 以上字段加 `{name,expectedBaseVersion,expectedPreviewSha256,approve:true}`，明确发布 |
| POST `/team-releases/:releaseId/git-update-preview` | `{expectedReleaseSha256}`，从已发布 Git 版本沿用资源选择，检查登记的默认分支 |
| POST `/team-releases/:releaseId/git-export-preview` | `{expectedReleaseSha256,changes:[{path,content,executable}]}`，生成未批准的二进制 Git 补丁；content 为 base64 |

在已发布 Git 版本详情中点击“检查登记分支更新”，服务端只解析该仓库
`DefaultBranch` 对应的完整 `refs/heads/...`。不会跟随远端 HEAD、同名 tag
或请求指定的其他 ref。根目录、资源选择与角色绑定沿用原版本。
没有新提交时不提供发布操作；有更新时显示原提交、新提交、检查时的项目版本
基线、完整配置、资源变化及未映射语义。即使只有未选文件变化，新的来源提交
也会明确显示并要求审阅。

批准更新仍调用 `/team-imports/publish`，在原固定提交输入中附加
`updateFrom:{releaseId,releaseSha256,ref}`。服务端核对其来源、资源选择、
审阅摘要及项目版本基线。新版本的可选 `gitImport.ref` 参与内容摘要；
旧版本缺少此字段时原有序列化及摘要不变。Runner 始终使用固定版本内容，
不会在执行时重新跟随分支。

检查更新在读取固定提交前后观察分支，批准时在重读内容前及发布前最后一次
再次观察。分支推进或消失、仓库 URL/默认分支改变、原版本撤销或项目版本
冲突都会拒绝；409 后保留输入及已展示内容，必须重新检查和审阅才能再次批准。
来源及登记状态在数据库锁内复核，成功只新增不可变版本，不覆盖 Catalog。
网络错误不会退回旧提交或缓存结果。

Git ref 与版本数据库没有跨系统原子事务：此流程保证发布审核的固定 SHA，
并在发布前最后一次观察到分支仍指向它；不能保证 SQL 提交瞬间分支仍未变化，
也不能检测两次观察之间的 A→B→A。后续托管写回需要单独的远端原子 ref
lease/CAS，不应将此只读观察流程当作该能力。

补丁预览只允许替换正式导入中已选择的现有资源文件，不增加/删除路径，不修改
teamai.yaml 或根目录许可文件。服务端重新读取原提交，验证完整清单及批准摘要，
再解析拟修改内容；不支持的配置不会生成补丁。补丁保留原始 blob 身份和模式，
路径包含配置根目录，因此未选仓库文件不受修改。响应的 `baseCommit` 是应用前
必须核对的基础提交；`proposed` 是待审核提案，不能直接作为正式版本执行。

本机真实 Git 测试已验证补丁应用、嵌套二进制、执行模式、未选文件保留及再次导入。
该接口不创建或推送分支、不创建 PR、不合并代码。登记分支检查更新已通过
本机真实 Git/MySQL/HTTP 验收，包括读取期间分支推进/删除的拒绝。实际浏览器
完成无更新、陈旧审批拒绝、重新检查及人工批准新版本。配置提案与草稿 PR 写回实现见下文；托管端真实验收、Git 导入到真实 Runner 的完整验收仍需单独完成。没有后台轮询
或自动发布；导入不可视为这些能力已经通过验收。

没有新表或启动数据迁移。回滚可停用导入/补丁入口，保留正式版本来源和审核历史；
撤销版本仍走现有正式版本治理流程。

## 人工审阅的配置提案与草稿 PR

补丁预览可显式“保存为配置提案”。服务端保留精确补丁、完整原始 Git tree、二进制与模式、未选文件、结果 commit 和 Git bundle，固定版本/基线/补丁摘要共同决定提案身份。项目 Owner/管理员读取原文与验证结果，按 revision + proposal SHA 明确批准；管理员再单独发起草稿 PR。保存、批准均不触发远端发布。

项目接口为 `/api/projects/:id/team-git-proposals`：GET 列表，POST 准备，GET `/:proposalId` 读取原文；POST `/:proposalId/decision` 审批，`/publish` 发布，`/reconcile` 恢复查找，`/cancel` 取消。准备请求冻结 `releaseId,expectedReleaseSha256,expectedBaseCommit,expectedPatchSha256,changes`；审批/发布使用 `expectedRevision,expectedProposalSha256`，发布还需 `expectedBaseCommit`。冲突后界面保留理由，必须重新读取审阅，不能自动批准新状态。

此路径沿用当前 Engine、仓库发布锁及持久化 outbox，显式 `sourceKind=teamai_proposal` 且 RunID 为空。窄验证策略 `teamai-configuration-proposal-v1` 证明配置可解析、资源/许可保留、选中路径限制和人工批准，不能当作代码 Run 的 Fast/Slow/独立 Review 验收。旧 contribution 仍使用原门禁。

PR 创建前先持久化一次性标记；响应不确定或旧 publishing 记录恢复时只查找原 PR，空结果不会触发另一次创建。响应必须确认 draft。取消、源版本撤销、登记/分支变化会阻止后续受控操作；已经发出的外部请求不能原子撤回。远端 ref 与 MySQL 无跨系统原子 CAS，本文不宣称该保证。

部署前人工评审并执行 `migrations/037_team_git_proposals.sql`，协调 publication 的空 RunID 兼容升级。真实 Git、MySQL 多连接、API/Engine/界面契约已有本机验证；专用最小权限凭据与真实托管 PR 验收仍待完成。该路径创建草稿 PR，不自动合并。
