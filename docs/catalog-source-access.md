# Catalog source read and copy boundary

## 1. Scope and trigger

When `AGENTFLOW_AUTH_REQUIRED=true`, project-owned catalog source content is
readable by the project owner, any project member (including viewers), or a
platform administrator. Explicit `Shared` definitions and generic templates keep
their public read behavior. Projectless platform presets retain their existing
public-read contract. Authentication-off is local compatibility mode.

This HTTP boundary controls source access. It does not grant execution authority
or introduce a new permissions/entitlement store. Formal-release execution and
historical snapshot access have their existing separate authorization.

## 2. Owning interfaces

The HTTP `deps` helpers in `catalog_read_access.go` derive identity/membership
from the existing auth and ProjectsRepo; there is no new registry or schema.

```go
func (d deps) catalogReadPredicate(c *gin.Context) func(projectID string, shared bool) bool
func (d deps) requireCatalogRead(c *gin.Context, projectID string, shared bool) bool
func catalogRevisionVisible(rev domain.DefinitionRevision, kind, entityID string,
    allowed func(string, bool) bool) bool
```

Catalog handlers remain the only source definition/detail/copy owners. MCP
handlers retain personal-MCP ownership; platform admin status does not reveal
another user's personal MCP under the existing personal visibility rule.

## 3. Routes and behavior

| Surface | Authenticated nonmember behavior for a private project asset |
| --- | --- |
| GET `/api/agents`, `/api/rules`, `/api/skills`, `/api/workflows` | Return 200 with inaccessible definitions filtered out; a `projectId` query never expands access |
| GET `/api/dockerfiles`, `/api/environments`, `/api/sandboxes`, `/api/mcp-servers` | Same source filtering; MCP public serializer still masks secret bindings |
| GET `/api/bootstrap` | Apply the same predicate before serializing source arrays, including Agent summaries; do not send hidden source bodies to the client |
| GET `/api/agents/:id`, `/api/workflows/:id` and their revision routes | Deny private source; historical revisions also require their own original read scope |
| GET `/api/skills/:id/files/*path`, `/api/mcp-servers/:id/files/*path` | Deny private raw-byte download |
| POST `/api/agents/:id/clone`, `/api/workflows/:id/clone` | Require source-read authority and existing target-write authority; Agent clone preflights referenced source closure before mutation |
| POST Agent/workflow revision restore | Existing target write checks plus exact revision kind/entity/payload identity; cannot restore another definition through a writable route |

Filtering applies to all returned source content, rather than hiding rows only
in the browser. Private read responses use `Cache-Control: private, no-store`.
Sharing a current definition does not publish its older private revision bodies.

## 4. Source snapshot and distribution rules

`SkillSource` means an installed read-only market snapshot. Existing upload and
file deletion continue to return 409 for sourced skills; only the market update
path can replace that snapshot. Ordinary definition metadata updates retain the
stored Source, Content, Files and binary manifest, ignoring forged replacements.

An explicitly shared skill can be read/downloaded under the existing sharing
policy. A release preserves its source metadata and selected bytes. If the source
owner later unshares the live skill, a recipient's authorized historical release
remains unchanged, while the live-source route denies that recipient. Snapshot
possession or execution authority is not checked as a source permission.

A shared Agent is not a blanket grant to copy every referenced private Rule,
Skill or MCP. The authenticated clone path preflights the full closure before
any share/create mutation. Foreign personal MCPs are rejected. Cross-scope
project MCPs carrying credential bindings are rejected rather than transferred;
an independently installed target MCP with the same market identity can be
reused without copying source bytes or credentials. The unauthenticated local
compatibility mode retains the existing local clone contract.

The current model has no independent license/no-copy/export-entitlement fields.
No such permission is inferred from a license string, frontmatter or successful
execution. This change does not claim a full distribution-policy engine.

## 5. Errors and compatibility

Unauthenticated protected API access retains existing 401 behavior. Inaccessible
private detail, binary or clone sources return 403 without source content.
Copying a credential-bound MCP to a different scope without an independent
target binding returns 409. Missing definitions return 404. Missing, foreign or malformed revision identity
returns 404. Source edit conflicts remain 409. Lists return filtered 200 arrays,
including an empty array when nothing is readable.

No data migration, seed or backfill is introduced. Existing shared/generic and
projectless platform presets remain readable. Rollback can remove the HTTP read
filter/guards without rewriting source or revision data; doing so restores the
older global-read exposure and is not a tenant-safe deployment policy.

## 6. Executed verification

`catalog_read_access_test.go` exercises private list/bootstrap/detail/history/
clone/download boundaries, owner/member/viewer/admin reads, explicit sharing,
generic visibility, old private history after current sharing, foreign revision
restore, source unshare after retained release, and SkillSource write protection.
`intelligence_test.go` checks release authority and actual stale publish/restore
responses separately. See the active task's `cap-release-review.md` for commands
and final package results; jsdom checks do not substitute for browser acceptance.

## 7. Correct and incorrect use

Correct: a viewer reads their project's skill resources, but cannot edit them.
A nonmember may read an explicitly shared skill, retain an owner-published formal
snapshot and still fail to read its live source after sharing is withdrawn.

Incorrect: a valid release ID unlocks all source project files; a writable Agent
URL authorizes restoring an unrelated revision; a client-side hidden row is
accepted as source isolation; or a market Source record is erased by a metadata
edit to bypass read-only ownership.
