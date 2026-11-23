# 原生资源的分发权限

Agent、Rule、Skill 和 MCP 可以分别约束平台提供的复制、内容编辑、显式导出操作。源码查看、执行授权、项目成员身份和 Shared 状态仍由各自已有规则决定。

这些权限管理应用操作，不提供 DRM、商业许可证认证或源码保密。已授权用户仍可以保存详情 JSON 或手工复制所见内容；已授权 Worker 的执行能力也不因此成为防泄漏沙箱。系统不识别任意手写或重新上传内容的隐藏来源。

## 模型与兼容

```json
{
  "distribution": { "copy": "allow", "edit": "deny", "export": "deny" },
  "distributionOrigins": [
    {
      "kind": "skill",
      "id": "review",
      "projectId": "source-project",
      "sourceRevision": "observed-source-token",
      "sha256": "source-definition-snapshot-sha256",
      "policy": { "copy": "allow", "edit": "deny", "export": "deny" }
    }
  ]
}
```

`distribution` 可省略，保留旧行为；设置时三个值必须完整且只能为 `allow` 或 `deny`。可选字段不会改变没有声明的旧 JSON 序列化，不回填历史数据。

`distributionOrigins` 由服务端的受控复制生成，最多 64 项，超过上限拒绝而不截断。来源记录包含当时定义快照的摘要和来源修订；MCP 摘要使用脱敏的公共定义，避免把凭据派生摘要暴露给客户端。每个操作取本层声明与全部继承策略的交集：任一来源禁止即为禁止。改资源标识、目标项目或资源所属 Owner 都不会删除来源底线。

来源策略对合法复制固定为复制时的快照。后来修改原资源不会重写已发布 release、Run 或复制的字节和摘要；撤销执行版本仍使用既有 release 撤销机制。

## 操作边界

| 操作 | 规则 |
| --- | --- |
| 标准 source/list/detail、已授权 release/Run/context JSON inspection | 保留已有读取权限；即使正文可以保存，也不额外受 `export` 限制 |
| Agent 复制、评测副本、模板采纳中实际新建的定义 | 检查实际来源 `copy`，并继承来源底线；独立已有目标资源复用或仅保留 Rule/Skill 引用不算复制正文 |
| 原生正文、元数据、文件修改、市场替换、恢复 Agent | 检查当前源 `edit` 与 CAS；省略治理字段不会清除它们，普通写不能改声明或伪造来源 |
| 删除或仅取消 Shared | 保留已有下线权限与 CAS；受限编辑不会让 Owner 无法撤回自己的资源 |
| Skill/MCP 原始文件 GET | 属于显式文件导出，检查 `export` 与原读取权限 |
| 专用 package/Git patch 导出 | 检查 frozen release 中的策略；Git 替换补丁还需内容 `edit`，不把 Git 元数据解释为原生许可 |
| 受控发布、更新、候选晋升 | 随对应定义保存来源与底线，已知来源不能借新 ID 或新 Git SHA 清空限制 |
| 内部 Run 冻结与 Runner claim 传输 | 执行实现，沿用原授权与撤销机制，不是额外分发权 |

普通详情不会重写不可变快照或返回伪装成原摘要的脱敏副本。本功能没有扩展为工作区 DLP 或任意执行结果内容识别。

## Owner 决策 API

| 资源 | 路径 |
| --- | --- |
| Agent | `PUT /api/agents/:id/distribution` |
| Rule | `PUT /api/rules/:id/distribution` |
| Skill | `PUT /api/skills/:id/distribution` |
| MCP | `PUT /api/mcp-servers/:id/distribution` |

```http
If-Match: "当前 sourceRevision"
Content-Type: application/json
```

```json
{
  "policy": { "copy": "allow", "edit": "deny", "export": "deny" },
  "reason": "记录此次权限决策的理由"
}
```

理由为 1–1000 UTF-8 字节。项目资源仅当前项目 Owner 或平台管理员可调整；个人 MCP 仅本人或平台管理员；无项目的原生资源仅平台管理员。普通 writer、viewer、外部用户或无人工身份的服务令牌不能使用此接口。关闭认证的本地模式沿用本地治理身份，但仍检查策略和 CAS。

成功返回 `kind`、`id`、新的 `sourceRevision`、`distribution`、`distributionOrigins` 和 `effectiveDistribution`，并设置新 ETag。缺少前提为 428，过期为 409，策略/身份拒绝为 403，无效策略为 400。Owner 可以显式调整自己的声明，即使内容 `edit=deny`；任何 Owner 或管理员均不能通过该接口放宽继承来源的禁止项。

项目行先于资源父行加锁，确认锁内最新 Owner，再进行源修订检查、权限决策记录和 token 轮转。决策使用现有 `definition_revisions`，类型 `distribution-<kind>`，实体为原资源 ID，记录身份、理由、旧声明和新声明，不新增权限服务或启动迁移。

正文/文件修改与策略修改使用相同父行与 source CAS。模板采纳在真实模板容器锁内整体事务执行，源策略拒绝或任一后续写失败均回滚整套新定义，不删除本来已存在的独立项目资产。

## 界面

Agent 详情、Rule/Skill 详情及 MCP 卡片/编辑窗口提供“分发权限”。所有可查看者可检查有效策略与继承来源；Owner/管理员可以填写三个明确选项及理由。继承禁止项不提供允许选项，服务端仍独立检查。

权限对话框冻结打开时的源基线。409 保留选择和理由，必须先读取并核对最新权限，再明确采用新基线，最后手工再次保存。权限保存不会静默改变其他正文草稿的编辑基线；应关闭并重新打开资源复核，避免丢失未保存编辑。
