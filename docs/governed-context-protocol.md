# Governed execution context protocol

The LoopForge Engine remains the sole dispatcher. TeamRelease and MemoryRepo
retain their resource ownership. This protocol extends the existing immutable
Runner assignment and P0 task-attempt contribution receipts; it adds no registry,
scheduler or parallel execution state.

## Frozen inputs and actual transport

The server resolves `executionContext.teamContext` IDs to `Run.intelligence`
(version 1). It contains the complete approved release payload, including skill
text, nested files, binary bytes/modes, selected approved memory revisions and
provenance, and exact repository/base SHA pins. The `sha256` field hashes that
snapshot with its own hash blank. `releaseSha256` identifies the immutable
configuration payload. Retry reads this retained Run snapshot, never the latest
catalog. An absent, malformed, changed, revoked or expired selected resource
fails; restoring a release publishes a new version rather than changing history.

Each executable governed task carries `invocation.governedContext`:

- `version: 1`, `intelligenceSHA256`, `projectId`, `releaseId`, `releaseSHA256`;
- `memories: [{id, revision, sha256}]`;
- `repositoryBaseSHAs` and optional `budgetPolicySHA256`.

This small binding references the retained Run snapshot. The actual selected
Agent/assets, prompt, repository pins, command/executor and delivery restrictions
already travel in `RunnerTask`. They, plus the governance binding, enter the
existing `RunnerContributionContextSHA`. We do not transmit another full copy of
the 8 MiB snapshot. Source credential fields are excluded by the existing public
assignment sanitizer; credentials are bound separately at execution time.

Task ID, Run ID and Node ID are explicit. The Node attempt and placement enter
version 2 P0 manifests when node placement is selected. The claimed task attempt
is fenced by the existing authenticated lease and appears in every evidence and
contribution receipt. A hash proves content identity, not current authorization.

## Admission and compatibility

Governed executable Agent, script, preparation and verification assignments require
Runner **0.12.0** or newer (strict SemVer). Cleanup actions carry no governance
binding. Ungoverned tasks retain existing compatibility.

The Engine checks live authorization before dispatch. Transactional claim checks
current Runner version and session, locks the release and then memory rows in
sorted ID order, and rechecks published/approved state, digest, memory revision
and expiry. A revoke committed before claim therefore denies claim. Existing
Run/task/registration locks and lease fences remain authoritative. Revocation
cannot erase already delivered knowledge or undo an in-flight external action;
later dispatch/claim is denied and no latest-content fallback is performed.

Placed task admission also compares the binding to its retained Run. Changed
repository pins, missing identity, invalid version/hash, oversized selected
Agent/assets, and a missing selected binding fail explicitly. New Runners verify
the entire P0 assignment digest before isolated checkout and before evidence
measurement. Older Runners cannot claim governed tasks through unauthenticated
legacy claim paths.

## Materialization and completion evidence

After `PrepareRemote` writes the actual selected resources, and **before** the
provider process launches, the Runner measures canonical regular files in the
isolated workspace. It compares their bytes, sizes and permission modes with the
selected runtime representation. `RunnerTask.outputs.contextEvidence` records:

- `version`, `taskId`, claimed `attempt`, `intelligenceSHA256`, `contextSHA256`;
- `files: [{kind, resourceId, path, sourceSHA256, sha256, size, mode}]`.

`sourceSHA256` hashes the original resource bytes. `sha256` hashes the generated
file consumed by the runtime. SKILL.md frontmatter may normalize, so these are
separate digests. Rules, including approved memory rendered as a rule, nested
text, binary resources and executable modes are measured. Only workspace-relative
managed paths and hashes are reported. Rendered MCP configs may contain private
credentials; they are not included in file measurements, while their approved
source definitions/bytes remain covered by the assignment manifest.

Scripts receive only their assigned asset subset (currently approved MCP source
configuration); they report a valid context identity and an empty file inventory
when no rules or skills were assigned. A terminal assignment with unmaterialized
rules/skills is rejected. Mock executors cannot claim actual materialization of
assigned instruction resources.

Successful completion requires context evidence. The existing completion lease
fences its task attempt; the server verifies its identity, source context SHA,
P0 context SHA, complete selected file inventory and source digests. It then
accepts immutable contribution receipts under the same context SHA. These
receipts bind exact repository base/result commits. Existing artifact metadata
links the task/Run output to the delivered artifact. The evidence is a Runner
measurement, not hardware attestation of an untrusted host. `run-context.json`
remains a debugging artifact and is insufficient as replay proof.

## Retention and rollback

Release payloads, governed memory, frozen Run intelligence, task output evidence
and accepted receipts follow existing durable database ownership/retention. No
new content garbage collector is introduced. Deletion/unavailability fails closed
instead of rebuilding from the live catalog. Disabling new Team context selection
allows legacy runs to continue; it does not silently downgrade an existing
selected Run or strip governance fields to let an old Runner execute it.

## Local acceptance scope

Protocol unit/integration tests cover legacy versions, missing/tampered identity,
source/generated skill hashes, nested binary bytes/modes, missing files, immutable
rule order, queued revocation/expiry, and attempt-bound completion evidence.
A real model/independent Linux consumption replay is a separate acceptance run;
passing these tests alone does not establish model invocation, remote-machine
acceptance or GitHub repository protection.
