# Controlled contribution delivery

This opt-in path extends the existing Engine, Runner protocol and domain Repos. It uses additive MySQL models and manually reviewed migration scripts028–031. Existing deployments must review/apply required SQL themselves; startup performs no data migration or seed.

## Assignment and accepted code

Create a controlled Runner Run with `runnerWorkspaceLayout: "attempt"` and `workspaceBaseSHAs: {"<repositoryId>": "<full-lowercase-40-hex-SHA>"}`. Every assigned repository needs a pin and Runner0.10.0 or newer. Each task/attempt receives an independent clone, branch and ownership marker. Unsupported interactive/child-SOP continuation fails explicitly. An optional node `maxDurationSec` (1..86400) freezes an absolute deadline for that execution attempt. Zero preserves legacy unbounded behavior; renewal/recovery cannot extend a frozen deadline.

Successful tasks export a standalone version2 SHA1 bundle with one prescribed ref, bounded to16MiB, before lease-fenced completion. The same RunnersRepo transaction accepts current-attempt receipt IDs and task success. Failure/cancel/expired attempts accept no contribution. Platform-issued hosting credentials and privileged MCP assets are excluded from Workers. Clean execution accounts and operating-system confinement remain deployment responsibilities.

## Controller API

All endpoints require a platform administrator when authentication is enabled. Runner sessions are not controller authority. The controlling/source Runs must have succeeded, remain uncanceled, belong to the same project, and retain frozen repository/base/context and latest accepted execution provenance.

| Method and path | Input / behavior |
|---|---|
| `POST /api/runs/:id/contributions/integrate` | `{repositoryId, taskIds}`;1..32 accepted tasks, deterministic ordered merge, immutable `prepared` snapshot. Conflicts reject. |
| `GET /api/runs/:id/contributions/:integrationId/content` | Revalidate provenance and stream verified aggregate Git bundle. |
| `POST /api/runs/:id/contributions/:integrationId/gates` | `{gateRunId,nodeId,kind,artifactId}`; derive evidence from an existing independent successful SOP Run. No caller status/SHA/review verdict is trusted. |
| `GET /api/runs/:id/contributions/:integrationId/publication` | Durable publication snapshot/history. |
| `POST /api/runs/:id/contributions/:integrationId/publish` | Consume fixed-SHA gates and controller-only project-owner hosting authority; create one draft PR, never merge. |

Absent integration returns404 at the HTTP ownership boundary. Provenance, gate, conflicts and missing publication prerequisites return409. Anonymous/non-admin callers are denied by authentication/admin enforcement.

## Fixed-SHA gates

Run verification through normal isolated SOP execution, pinned to the integration result SHA. Each gate task must accept a no-change receipt whose base and result both equal that SHA. Tests/review must put reports in excluded control paths (for example `.agentflow/reports/`) and publish an immutable attempt-scoped artifact. Modifying product code invalidates the gate.

`fast` and `slow` require distinct successful terminal script tasks and report artifacts. `review` requires a distinct Agent task, nonmock/nonterminal provider, model session, passing invocation-scoped GateResult, no high/critical finding, and a reviewer AgentID different from every source author. The control plane hashes recorded task/node/frozen workflow/report facts into evidence tied to integration SHA/context/manifest. It rederives those facts before publication; canceled, superseded, tampered or missing reports are rejected. Administrators own meaningful command/review policy: a successful trivial script does not prove adequate test coverage, and a separate AgentID is process/role separation, not an independent model vendor or person.

## Outbox and remote writes

DeliveryRepo stores immutable integration rows and a durable publication row. All three gate kinds must pass before `ready`. A publisher claims a bounded attempt/owner lease under the existing repository integration lock; stale claims cannot complete. The controller reconstructs clean Git objects again, checks gates/lifecycle before each remote operation, and uses the frozen GitHub repository identity/default target. Only controller-side project-owner hosting authority is used; no hosting key is sent to the Runner.

The branch is `superagent/integration/<manifest-id>`. The GitHub Git Data API adapter first checks the real target history contains the frozen base, uploads dependency-ordered blob/tree/commit objects and verifies each returned SHA. It creates the branch only after the exact result object exists; existing identical SHA is reusable and a different SHA is never overwritten. Unknown/signed/noncanonical commit headers that cannot be recreated byte-for-byte are rejected. The native Git HTTPS transport remains a fallback when no provider object adapter is bound; it also checks target ancestry and uses an empty expected-ref lease for create-only semantics. The GitHub adapter verifies branch SHA, searches all PR states for that stable branch, creates a draft only if absent, reconciles an uncertain POST result, and rechecks the open/unmerged head/target with auto-merge absent. It never calls merge or enables auto-merge. Before the first PR adapter call, `DeliveryRepo.MarkContributionPRCreateAttempted` durably records `phaseVersion:1` and `prCreateAttempted:true` under the current publisher owner/attempt/lease fence. That is one automatic create authorization. After any response loss, cancellation, expiry or process restart, the marker remains sticky: reclaims can call only `FindOpenBranchPRAtSHA`, whose absent/eventually invisible result never sends POST. Discovery verifies the same fixed branch/head/target and rejects closed, merged or auto-merge-enabled PRs. No automatic reset or replacement PR is authorized. A crash after recording intent but before sending POST deliberately sacrifices automatic recovery; an operator must investigate rather than silently retry creation. Old `publishing` payloads without phase metadata conservatively upgrade to discovery-only, while old unstarted `ready` rows may perform their first create. This is a JSON payload extension, with no new table, scheduler or automatic migration.

A completed `pr_ready` replay returns the same historical receipt and cannot create a replacement after human closure. This snapshot is not a live PR-state poll.

A published PR awaits human review/merge and does not complete an Issue. GitHub branch protection, approval requirements and least-privilege token policy must be configured/verified independently for a production installation; this code does not install those repository policies.

## Explicit limits

Local Mac/Docker Linux acceptance does not establish physically separate cloud-host/network acceptance. Attempt folders are same-user code separation, not hostile tenant confinement. Native owner-death supervision covers the original process group; cooperative CLI cancellation can stop its separate tool groups, but malicious `setsid` escapes and bwrap/Docker supervision need independent containment.

Git bytes/hash/ref/ancestry/object-size checks do not cap Git's temporary pack inflation memory/disk; hostile bundle processing needs OS/container resource limits. Contribution blob retention/GC and production packet sizing need operations work. Cancellation/read-time source checks and remote GitHub writes are not one atomic transaction: cancellation cannot undo a write already in flight. Stable identity/outbox reconciliation preserves recoverable effects, but no distributed exactly-once or instantaneous revocation guarantee is claimed.

Absolute wall-clock limits, cumulative metered token/cost accounting and objective no-progress stopping are implemented in the existing Engine; see [bounded execution](bounded-execution.md) for the support matrix. Current adapters refuse strict provider-backed token/cost reservations before execution. Contribution publication itself does not run a repair loop: a failed gate stops publication, and a new integration requires fresh evidence.
