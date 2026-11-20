# Governed team configuration and execution context

This opt-in extension uses the existing CatalogRepo, Engine and Runner; it does
not run a second scheduler or install an external memory service. Formal team
configuration and reviewed experience have separate owners and lifecycles.

## Formal releases

A project owner or platform administrator can publish a named version from
existing project/shared Agents, Rules, Skills and project MCP definitions.
Publication captures the exact selected Agent resource closure, text files,
binary bytes, file digests and mode bits. Catalog edits never mutate a release.
The release digest includes all immutable content. Structured MCP environment,
headers, authentication bindings and credential-bearing URLs are rejected;
credentials must be configured outside the portable release. Dynamic platform
or knowledge MCP flags on released Agents are rejected: a governed execution
must not silently retrieve unpinned configuration or knowledge.

`CatalogRepo` owns releases. Versions increase within a project, serialized by
the existing project row lock. Publish and restore require the last observed
`expectedBaseVersion` (0 only for the first publication). A second writer using
the same base receives 409 and creates no version; revoked rows still count
toward the project version. The web form retains its input on conflict and
requires a refresh and explicit retry. Revocation preserves payload and decision history.
Restoring a historical version publishes a new explicitly approved version with
the exact historical bytes; it never reactivates a revoked row or changes a Run.
For native publication, repository/commit/path provenance remains supplied metadata.
The separate [TeamAI Git adapter](teamai-git.md) verifies selected source blobs at
a registered repository commit before explicit publication, recording a complete
selected-file manifest. Automatic synchronization and hosted PR writeback remain
separate work; this is a bounded basic-format importer, not full CLI compatibility.

Formal skill validation rejects path traversal, backslashes/NUL, whitespace and
case aliases, file/directory collisions, duplicate text/binary files, invalid
UTF-8 instructions/entrypoints, special/symlink mode bits, oversized packages,
and IDs that collide under the runtime's lowercase/sanitize/48-byte naming.
Selected LICENSE and NOTICE resources remain exact package bytes. These bytes
record supplied notices; publication does not independently certify license
compatibility or retrieve an upstream repository.

New explicit generic-template adoption records the original default and actual
project-remapped copy in the existing `definition_revisions` storage. The
`default-upgrades` endpoint compares those immutable baselines with current
defaults and current team overrides, including JSON-pointer conflicts and full
inspectable content. It performs no merge and does not replace local edits.
Re-adoption preserves the original baseline; older copies without provenance
remain `tracked:false`, with no startup/read-time backfill. A removed upstream
resource is reported as unavailable. Malformed stored baselines fail closed.

## Governed memory

`MemoryRepo` owns native project-scoped memory. Valid submissions always become
`candidate`; caller-supplied state, revision and decision identities are rejected.
Only a project owner or a
platform administrator can approve or reject a candidate, or revoke an approved
entry. Each decision requires the expected revision and a reason. A rejected or
revoked entry cannot be republished in place; submit a new reviewed candidate.
Content/source/expiry are immutable. Run/artifact references are checked against
the same project when a candidate is created.

Recall is deterministic bounded lexical search over at most 500 recent approved
entries. Candidates, rejected/revoked entries and expired entries are ineligible.
Recall has no shared cache, so a completed revoke takes effect on the next read.
The API requires project membership for reading/submitting and owner/admin
identity for decisions when authentication is enabled. Service/Runner sessions
without a user identity do not acquire governance authority. Authentication-off
local operation explicitly uses actor `local` and is not tenant isolation proof.
This is a native memory provider, not a claim of TencentDB Memory or CodeGraph
integration, semantic recall quality or an external cloud deployment.

Limits: candidate content 16 KiB, recall 10 entries/64 KiB, release/context JSON
8 MiB. Lists are bounded; recall is not an exhaustive unbounded corpus search.

## Pinned execution

A normal Run request may select:

```json
{
  "teamContext": {
    "releaseId": "team-<id>",
    "memoryIds": ["memory-<id>"]
  }
}
```

Use the existing project and exact `workspaceBaseSHAs` for every assigned
repository. The server resolves IDs into `Run.intelligence`, including formal
resource bytes, approved memory versions, repository pins and a context digest.
Clients cannot supply a trusted snapshot. All Agent nodes must exist in the
release; child SOP context inheritance is currently rejected explicitly.

The normal Engine Agent path uses the pinned role prompt and exact asset bundle.
Reviewed memory enters the selected invocation as a source/digest-labelled rule.
Binary skill resources materialize with their recorded bytes and modes.
The existing runtime deterministically adds/aligns SKILL.md frontmatter to the
sanitized skill ID; the formal source digest covers the original package, not
these effective normalized entrypoint bytes. A matching frontmatter entrypoint
is preserved byte-for-byte. Source and effective runtime digests must therefore
be interpreted separately; no external CLI file format is implied. Later catalog
edits, retries and database reloads do not select latest content. Missing,
corrupt, revoked, expired or changed evidence fails validation. Revalidation is
performed before node/invocation execution and controlled publication; it does
not erase knowledge already delivered to an in-flight external model process.
Legacy Runs without a selection retain their existing asset-resolution behavior.

Script/command nodes, workspace preparation and Runner verification payloads
also consume the pinned release MCP set. They never include personal MCPs or
later catalog additions. Remote Agent MCP calls use only that Agent's pinned
asset set. The controlled delivery policy continues to remove every MCP
capability; selecting a formal release does not bypass that restriction.

## API

All routes below are under `/api/projects/:id`:

| Method and suffix | Operation |
| --- | --- |
| GET `/team-releases` | Bounded version metadata |
| POST `/team-releases` | Publish `{expectedBaseVersion,name,agentIds,ruleIds,skillIds,mcpServerIds,source}` |
| GET `/team-releases/:releaseId` | Exact scoped configuration snapshot |
| POST `/team-releases/:releaseId/revoke` | `{expectedRevision,reason}` |
| POST `/team-releases/:releaseId/restore` | Publish historical bytes as a new `{expectedBaseVersion,name,reason}` version |
| GET `/default-upgrades` | Read-only original/default/team comparison; no automatic merge |
| GET `/memories` | Scoped bounded review queue/history |
| POST `/memories` | Candidate `{title,content,source,expiresAt?}` |
| POST `/memories/:memoryId/decision` | `{decision,expectedRevision,reason}` |
| POST `/memory-recall` | Approved-only `{query,limit?}` |
| POST `/context/resolve` | Preview `{releaseId,memoryIds,repositoryBaseSHAs}` |

Lifecycle/revision conflicts return 409 with `code: "intelligence_conflict"`, unavailable/foreign resources return
404, malformed input returns 400, and unauthorized identities return 401/403.
The routes enforce their own project scope; possession of an ID grants no access.
Memory submission, decision, recall and context-resolution bodies must contain
exactly one JSON object, at most 1 MiB, with only the documented fields (including
nested source fields). Unknown identity/scope/state fields return 400 rather than
being silently ignored. Project scope and decision actor come from the route and
authenticated user; request bodies cannot select another user/team/session scope.
The same project membership check protects all detail, stream, log, artifact and
workspace routes under `/api/runs/:id` for Runs with a selected governed context.
Global and issue Run summaries omit the intelligence payload. Runner sessions
retain only their existing task transport authority and cannot read governed
Run detail as a project user.

## Deployment and verification

Review/apply `migrations/035_governed_intelligence.sql` manually for existing
MySQL installations. It creates formal-release and memory tables and adds the
Run intelligence JSON column. Startup has no data migration or seed. A reversible
rollback disables new context selection while retaining audit/history rows.

Focused tests cover exact text/binary configuration freezing, monotonic version
and rollback publication, digest tampering, source/project scope, human decision
authority, stale revision, recall lifecycle/limits, database context roundtrip,
missing-manifest rejection and revocation. An Engine approval/Agent/approval test
edits live catalog resources after assignment and checks the executed role output
and materialized original rule plus approved memory. A real local shell adapter
reads materialized pinned text/binary resources. These tests establish local
contract/execution behavior, not a live-model recall quality evaluation or an
external memory-provider deployment.

The disposable MySQL release acceptance runs two independent Store/connection
pools against a newly generated schema in the explicitly named local test
container. Concurrent same-base publish and restore each admit exactly one
winner and one stale conflict, with original revoked history retained. Run it
using `scripts/test-execution-budget-mysql.py --container <disposable-name>
--suite releases --report <sanitized-evidence.json>`; the wrapper cleans its
random schema and temporary SQL user and never reports a DSN or credentials.

## Source, candidates and remaining scope

Project-owned source definitions, binary downloads, Agent/workflow history and
clone sources require owner/member/admin identity when auth is enabled.
Lists/bootstrap use the same filtering predicate; explicit shared assets and
generic templates remain readable. Later sharing does not expose private
historical bodies. Platform presets without a project retain their public read
policy. Personal MCP visibility is unchanged. Market sources remain read-only;
metadata edits cannot strip provenance or replace their resource files. Formal
releases do not grant authority to read, copy or edit a live source after sharing
ends. See [Catalog source access](catalog-source-access.md).

Native mutable Agent/Rule/Skill/MCP writers now require the observed opaque
sourceRevision via If-Match; a missing base returns 428, a stale base 409. File
and metadata writes share the parent revision. This differs from formal release
publication CAS. Existing mutation clients must be upgraded together; do not run
old unconditional writers alongside the new API. Cross-process acceptance and
complete legacy-client regression results belong to the recorded validation
cohort, not this API description.

[Skill candidates](skill-candidates.md) retain quarantined original packages,
source, license/notices, file digests and modes. Explicit human approval creates
a new project skill atomically without overwriting existing identities; formal
team publication is still a separate decision. Extraction/model output alone
cannot make a candidate executable.

The [TeamAI Git adapter](teamai-git.md) imports reviewed basic-source configuration
and previews patches that preserve unselected files. Automatic synchronization,
hosted branch/PR writeback, independent license/copy/export entitlements beyond
current source authority, and a production external Memory provider remain open.
Source visibility is not proof of commercial permission; retained license text
is not an automatically granted export entitlement.
