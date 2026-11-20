# 从固定模型产物生成知识候选

普通 Agent SOP 可输出 `knowledge-candidates.json`。项目成员在“团队配置与记忆 → 从运行产物生成候选”中读取成功运行，选择固定产物并明确生成候选。服务端读取保留字节，核对有效 task、attempt、预算记录、隔离贡献与仓库提交；模型正文不能自报这些事实。

此入口只消费现有 Engine/Runner 的结果，不运行模型、不建立调度器。每个批次最多八项、256 KiB，支持记忆与纯文本技能，全部写入成功或全部回滚。记忆保持 candidate，技能保持 quarantined；原有 Owner/管理员人审、Memory 索引同步和团队版本发布各自独立。

## 首版支持范围

来源必须是单仓库、固定完整 commit、成功且未取消的受控隔离 Run，带冻结正式团队上下文。提炼节点为 Agent、`maxAttempts=1`，通过现有 `nodePlacements` 绑定 Worker；必须是当前 placed author task，Node/task attempt、sequence、执行任务指针和冻结 placement 全部一致。Run 预算限 1..8 次调用、1..262144 total tokens；任务实际完成不得超出冻结截止时间，截止时间距节点开始最多 10 分钟。建议本机实验用 metered / 131072 tokens / 1 invocation / 180 秒。实际接受的贡献 base/result 均须等于所选 commit，模型不能通过此入口把代码修改当成只读提炼。

首版仅接受没有禁止分发项、没有继承来源记录的冻结输入。带受限资源、前驱产物或未建立受控来源清单的输入会明确拒绝。该范围不支持一般受限 Memory 的派生权限传递；旧记忆没有声明的限制也不会被推断成法律许可。

已批准记忆若显式关联来源 Run，必须同时关联服务端提炼凭证；手工登记的 Run 来源不能证明无限制输入，因此不支持再次提炼。服务端在事务中锁定本项目原凭证，校验其摘要、`v1-unrestricted-inputs-only` 策略、准确 Run／产物／仓库／提交／路径，以及该记忆的候选 ID 与内容 SHA。没有 Run／提炼关联的原生人审记忆仍可作为独立人工声明使用；该边界不递归追溯其他 Run，也不把人工批准等同于取得受限来源的分发许可。

输出契约须为 required `application/json`，文件名为 `knowledge-candidates.json`，建议路径 `.agentflow/artifacts/knowledge-candidates.json`。服务端同时复核保留 Artifact 原文的大小/SHA 与 task 的声明 OutputFiles / Outputs.Files exact bytes。Runner 必须支持既有 governed context（至少 0.12.0）；若来源上下文带 Memory recall receipt，则至少 0.13.0。无需新 Runner RPC。

建议输出：

```json
{
  "schemaVersion": 1,
  "suggestions": [
    {
      "key": "retry-rule",
      "kind": "memory",
      "title": "恢复时沿用累计用量",
      "content": "重试不得清空已记录的调用用量。",
      "evidencePaths": ["src/budget.go"],
      "licenseClaim": "unreviewed"
    },
    {
      "key": "review-budget",
      "kind": "skill",
      "skillId": "review-budget",
      "description": "核对预算恢复行为",
      "files": {
        "SKILL.md": "---\nname: review-budget\ndescription: 核对预算恢复行为\n---\n\n核对累计调用用量不会被恢复操作清零。\n",
        "NOTICE.md": "Generated suggestion; source and license require human review.\n"
      },
      "noticePaths": ["NOTICE.md"],
      "evidencePaths": ["src/budget.go"],
      "licenseClaim": "unreviewed"
    }
  ]
}
```

未知字段、重复 JSON 键、尾随 JSON、越界文件路径、二进制和可执行文件权限均不支持（技能资源以文本及普通 0644 文件保留）。每项记忆标题最多 200 bytes、正文最多 16 KiB；每项技能最多八个文本文件、合计 64 KiB。`licenseClaim` 最多 1988 bytes，以容纳存入技能包时的 `UNREVIEWED: ` 前缀；证据路径最多 16 个、每个最多 512 bytes。技能仍须通过完整技能包校验。`licenseClaim` 和 `evidencePaths` 是模型的未核验声明；保留的来源凭证明确记录这一点，不表示事实或许可已验证。

## 接口与来源

`POST /api/projects/:id/knowledge-extractions` 只接受：

```json
{
  "runId": "实际成功运行 ID",
  "nodeId": "实际 Agent 节点 ID",
  "artifactId": "固定产物 ID",
  "expectedArtifactSHA256": "产物的完整 SHA256",
  "repositoryId": "固定仓库 ID",
  "expectedCommit": "固定完整 Git SHA"
}
```

请求正文最多 8 KiB。新建返回 201，重复来源返回 200，响应为 `{receipt,replayed}`。幂等身份绑定 Run generation、有效节点/task attempt、固定产物与仓库提交，独立于提交人和时间。HTTP 前置认证后仍会在持有项目锁时复核当前用户、Owner/成员/管理员身份，不能借锁等待前的旧成员权限导入。重复导入不会重置审核决定或新建候选。源状态失效时拒绝生成；历史凭证仍可通过 `GET /api/projects/:id/knowledge-extractions/:extractionId` 读取。

候选的可选 `source.extractionId` 由服务端写入并参与其摘要，手工提交不能伪造。来源凭证保存真实任务、配置/观测模型的区别、会话、冻结团队版本、资源上下文、产物及用量记录。观测不到费用时保留 unknown，不能用零费用替代。

错误包含稳定 `code`：格式错误 400 `extraction_invalid`；受限输入 403 `extraction_restricted_inputs`；锁内成员权限已改变 403 `extraction_membership_changed`；固定来源失效 409 `extraction_source_conflict`；来源/凭证不在项目中 404 `extraction_unavailable`。单次导入有 10 秒请求超时；相同固定来源可安全重试，超时不启动模型。

## 部署与验证边界

现有部署须先人工评审并执行 migration 039；启动不运行 seed、数据回填或提炼任务。回滚可关闭入口并保留候选及审计凭证。

解析、幂等、事务、权限和固定来源需要分别验证；无模型测试不能作为真实提炼质量证明。实际模型提炼、人审、BM25 召回、新 Runner 消费及同任务对照需记录各自的事实、时延、token 与 unknown 费用。单例结果不建立普遍知识质量结论。CLI 的 metered 用量停止仍不等于供应商强制的费用预留。

2026-11-18 的本机合成材料实验完成了真实 Codex 提炼、原子导入和幂等重放、未审核不可召回、明确授权的 API Owner 合成材料审核、私有 BM25 凭证与全新 Runner 消费。三次调用各限 131072 tokens／180 秒，均只调用一次；两组消费结果均为 5/5 正确。这是测试材料的审核与执行验证，不是业务人工验收或上游许可确认。

| 阶段 | 实测 Token | Run 耗时 |
| --- | ---: | ---: |
| 提炼 | 75811 | 42.256 秒 |
| 无记忆消费 | 111773 | 35.031 秒 |
| 带记忆消费 | 92106 | 27.077 秒 |

本例带记忆消费少用 19667 tokens，但提炼加一次带记忆消费共 167917 tokens，高于一次无记忆消费；所有费用均为 unknown。单例、执行顺序与缓存因素不足以建立因果或普遍节费结论。保持按需使用与逐项审核，不据此开启自动提炼。此前失败实验、修复前后回归和资源清理按各自证据批次保留。
