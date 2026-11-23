# SuperAgent development baseline

Baseline: SA-BL-2026-11-15-v1. The [structured architecture archive](sources/SuperAgent_全轮技术架构讨论归档.md) is preserved byte-for-byte (SHA256 `417a1eb57274eb17e19ccbe9f09e30e21739c93c17b5a32837f9ace500a01694`). It is design source material; pseudocode, proposed schemas and source assertions in it require verification against this checkout.

## Product goal and decisions

Turn an approved requirement into isolated, traceable code changes, verified evidence and a PR. Reuse LoopForge as the sole workflow authority, retain its Runner task/event/lease protocol and domain-owned MySQL persistence, and support personal local plus shared Linux execution. Humans accept and merge; a successful Run or open PR does not complete a business work item.

The source foundation is LoopForge commit `c87694a3d9fee16dc399f4cd5331cd47b1b72cef`. Preserve its MIT and third-party notices. Retire the earlier Python application from the target production runtime while retaining its recoverable source, contracts and behavioral examples separately. Do not port a competing scheduler, queue, state machine or database merely to preserve prior implementation effort.

Development tasks use the existing Trellis management workspace. Source synchronization, raw machine/model traces, credentials and recovery archives remain local. The product contains portable goals/contracts and reproducible opt-in tests; it does not duplicate the management task/session authority.

## P0 evidence plan

| Task | Owning question | Reproducible source |
|---|---|---|
| SA-P0-001 | Fixed source, license, real extension owners | Root LICENSE/THIRD_PARTY_NOTICES and domain Repo/handler layout |
| SA-P0-002 | Actual native Runner/Codex implementation, human gate, cancellation/deadline | `e2e/api/38-codex-baseline.test.mjs` |
| SA-P0-003 | Workspace/Git separation across reuse, parallelism and cancel/retry | `e2e/api/39-sandbox-isolation.test.mjs` |
| SA-P0-004 | Attempt fencing, events and real disconnect/owner-death recovery | `e2e/api/36-runner-attempt-fencing.test.mjs`, `37-runner-recovery.test.mjs` |
| SA-P0-005 | Mac/Linux accepted commits, deleted sources, fresh offline reconstruction | `e2e/api/40-contribution-handoff.test.mjs` |
| SA-P0-006 | Fixed-SHA Fast/Slow/independent model review and create-only draft PR | `e2e/api/41-contribution-publication.test.mjs` |
| SA-P0-007 | Evidence-backed reuse/extension verdict and bounded next phase | Local assessment distinguishes passes, failures and unverified deployment conditions |

Opt-in test files may skip without their explicit prerequisites. A skipped model/GitHub/MySQL test is not proof of that capability. Existing `e2e/local.sh` creates disposable infrastructure; never aim these experiments at production data. Use an explicitly authorized disposable GitHub repository and owner-only external credential files for live publication.

## Established implementation scope

Protocol-v2 task ownership includes the immutable attempt; stale start/renew/events/complete/control and post-lock expired mutations reject. Event sequence identity includes attempt. Controlled delivery strips platform hosting authority from Workers and disables legacy automatic merge/Issue completion. Optional attempt workspaces freeze exact base/context and export immutable accepted Git bundles. The same Engine reconstructs/integrates code and consumes independent SOP gate evidence through [controlled contribution delivery](../contribution-delivery.md).

These describe the P0 checkpoint. Later follow-ups add explicit Node placement, immutable native team/resource/memory snapshots, a bounded repair ledger and visual controlled delivery. TeamAI basic-source import/review and the optional private memory index have local integration evidence; full upstream TeamAI compatibility and production acceptance remain open. See [current implementation status](implementation-status.md) for the authoritative implemented scope and verification. Provider-enforced hard token/cost reservations, hostile tenant/process/pack containment and production operations remain separate acceptance work. Local Docker Linux execution does not establish an external physical cloud-host acceptance.

## Next-phase gate

Assess all six original criteria: one Engine, real recoverable Workers, viable code isolation, stale-attempt rejection, traceable Git integration, human merge. Preserve failed baseline trials and report local versus production acceptance separately. Continue with narrow workspace/integration/placement extensions only where measured gaps justify them; reconsider the foundation if its core execution model needs replacement. Do not expand console/memory scope to conceal unresolved execution or delivery guarantees.
