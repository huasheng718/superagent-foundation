# Catalog source edit concurrency

Agent, Rule, Skill and project/personal MCP definitions expose a server-owned
`sourceRevision`. It identifies the editable source state, independently of an
immutable team release version or an Agent's display version.

## HTTP clients

Existing definitions require a strong quoted `If-Match` header on metadata
PUT, whole delete, file upload/delete, Agent history restore and SkillHub update:

```http
If-Match: "<sourceRevision from the reviewed response>"
```

New PUT definitions use `If-None-Match: *`. For compatibility, an unconditioned
PUT can create an absent ID; it cannot overwrite an existing ID. The body field
`sourceRevision` does not substitute for the header. Weak, wildcard or multiple
If-Match values are invalid. Importing MCP JSON is an atomic **create-only** batch;
existing identifiers must be edited separately. Clone/install paths also create
without overwriting an existing target.

| Result | Meaning |
| --- | --- |
| 428 `source_precondition_required` | Existing edit omitted its reviewed base |
| 409 `source_edit_conflict` | The reviewed base is stale, or create ID collided |
| 400 | Invalid conditional header, or unsupported mixed Agent-evaluation batch |

Authorization, private-source filtering and existing not-found handling still
apply. Error responses do not disclose the latest source contents. Local
authentication-off mode still enforces edit preconditions.

Successful writes return the committed definition and its new revision. Retain
that response for the next operation; do not infer the token from timestamps or
masked MCP credentials. Skill/MCP file writes and metadata share the same token.
A metadata save followed by an upload is two explicit operations: if upload
fails, the accepted metadata save remains accepted.

## Review behavior

Editors freeze their base when opened. A background catalog refresh does not
advance an unsaved form. On conflict the editor preserves the draft; the user
compares authorized latest contents, explicitly adopts that reviewed base, then
submits again. No automatic merge or retry occurs. Skill uploads retain selected
browser File objects on conflict; after explicit comparison a separate button
retries those files. SKILL.md replacement asks for its destructive confirmation
again. Leaving that asset clears the retained selection.

Agent evaluation apply sends `expectedAgentRevisions` for the reviewed source
and any existing project-copy target. A request containing Agent changes applies
one recommendation at a time; mixed or multiple mutations are rejected before
any write. Workflow-only evaluation retains its own contract.

## Storage and deployment

CatalogRepo locks the actual parent row and compares the base in the same
transaction as child files, manifests, Agent history and token rotation.
Intentional Agent sharing updates owned Rule/Skill sharing in that transaction;
ordinary internal Agent writes never grant source permissions. Generic bundled
Agent edits lock their existing template container, as do generic Workflow
updates/publish, preserving each other's accepted halves.

Existing rows receive deterministic compatibility tokens on read, without
backfill. New/changed rows receive random tokens inside existing JSON payloads;
no SQL migration is required for this field. SkillSource files remain read-only
outside reviewed market updates, and MCP masked credentials are restored from
the locked existing record. Agent restore creates a new token/version/history;
it never reuses the historical editing base.

Upgrade UI and all API writers together under write maintenance. Mixed old/new
writer binaries are unsupported because an old writer can bypass this contract.
Rollback must keep source writes disabled until the API/UI cohort is consistent.
Do not add a fetch-latest-and-retry adapter to old clients.

## Verification boundaries

Focused Repo and HTTP tests cover stale/missing/header/body, file manifests,
restore, generic container preservation, clone rollback, explicit sharing and
evaluation negatives. `webtest/tests/source-edit-cas.test.mjs` covers draft/base
retention, reviewed retry and masked comparison. Opt-in
`e2e/api/45-source-edit-multi-instance.test.mjs` requires two distinct local API
processes and an explicitly authorized disposable MySQL instance; it checks
same-base races and cleanup. This is local concurrency evidence, not physical
multi-host, hosted GitHub policy, full binary-history retention or independent
copy/export entitlement acceptance.
