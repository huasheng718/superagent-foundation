# Reviewed Skill candidates

Project members can submit a complete extracted or imported Skill package to a
quarantine queue in the project's Team configuration panel. Only the project
owner or a platform administrator can approve, reject or withdraw a pending
candidate. Approval atomically creates a new project Skill; an existing Skill is
never overwritten. Publishing a formal Team release is a separate explicit
action.

## Package and review

The API accepts regular named byte records, not local directory paths or archive
extraction requests. Text resources are in `skill.files`. Binary resources are
Base64 values in `binary` with exact path, size, SHA256 and mode entries in
`skill.fileRefs`. Normal file modes 0644 and executable 0755 are preserved.
`SKILL.md` must have closed frontmatter with a name matching the canonical Skill
ID and a nonempty description. IDs use lowercase letters, numbers and hyphens,
are at most 48 bytes, and cannot begin or end with a hyphen.

Every package needs source provenance, publisher, a license record and one to
16 existing `noticePaths`. Preserve the original LICENSE/NOTICE and all selected
nested text and binary bytes. License metadata records supplied evidence; it
does not independently grant copying, sharing or export rights. Source
Run/artifact references must belong to the current project.

For example, a text-only candidate can be submitted as:

```json
{
  "skill": {
    "id": "reviewed-example",
    "name": "Reviewed example",
    "files": {
      "SKILL.md": "---\nname: reviewed-example\ndescription: Read the reviewed reference.\n---\n\nRead references/context.md.\n",
      "LICENSE": "The actual original license and copyright notice.\n",
      "references/context.md": "The original reference bytes.\n"
    }
  },
  "source": {
    "repository": "https://example.com/team/source",
    "commit": "1111111111111111111111111111111111111111",
    "path": "skills/reviewed-example"
  },
  "publisher": "The actual source publisher",
  "license": "The actual license identifier or authorization record",
  "noticePaths": ["LICENSE"]
}
```

The UI supports pasting JSON or selecting a JSON file. Before approval, the
reviewer loads the retained original entrypoint, nested contents, source,
license notices, resource hashes, sizes and executable modes. Approval requires
an explicit reason and the observed lifecycle revision. A conflict is shown as
an error and requires refreshing and reviewing current state.

## Lifecycle and API

All routes are scoped under `/api/projects/:id`:

| Method and suffix | Operation |
| --- | --- |
| GET `/skill-candidates` | At most 100 metadata records; no raw package bytes |
| POST `/skill-candidates` | Submit a `SkillCandidatePackage`, always quarantined at revision 1 |
| GET `/skill-candidates/:candidateId` | Exact retained package, derived resource inventory and decisions |
| POST `/skill-candidates/:candidateId/decision` | `{expectedRevision,decision,reason}`; decision is `approved`, `rejected` or `withdrawn` |

Authentication and project membership protect submissions and reads. Human
owner/admin identity protects decisions; a Runner transport session does not
gain review authority. Local authentication-off operation uses the existing
local governance identity and is not evidence of tenant isolation.

Candidate content is immutable. Revising a package means submitting another
candidate, optionally with a same-project `supersedesId`. Only quarantined rows
can be decided. Rejected, withdrawn and approved rows remain audit history;
there is no delete, reset or content-update route. An approved candidate records
the promoted Skill ID and provenance. Its retained package digest stays fixed.
An approved decision cannot subsequently erase a Skill, revoke a formal release
or retract bytes already delivered to a model. Those assets have separate
governance lifecycles.

Malformed packages return 400, absent or foreign resources return 404,
unauthorized identities return 401/403, and stale revisions or active Skill
identity collisions return 409 with `code: intelligence_conflict`. Project and
actor are server-owned. The package must not supply a Skill source revision,
market source, sharing flag or update timestamp. Runtime-normalized Skill name
collisions are rejected before promotion. Candidate decisions and Skill/binary
creation share one transaction; failure leaves quarantine and active assets
unchanged.

## Deployment and verification

Operators review and manually apply `migrations/036_skill_candidates.sql` for
existing MySQL installations. Startup only uses the repository's existing
incremental DDL mechanism; no seed, data migration or extraction service is
introduced. Disable candidate routes for rollback while retaining audit rows.

Focused Go tests cover isolation, real authenticated roles, cross-project
provenance, malformed paths/digests/modes/UTF-8, exact bytes/modes/notices,
create-only collisions, stale revisions and transaction rollback. UI contract
tests cover the real API workflow and conflict/error handling. The opt-in MySQL
test uses two independent Store connection pools and a random disposable schema
to race the same decision:

```sh
python3 scripts/test-execution-budget-mysql.py \
  --container <authorized-local-disposable-mysql> --suite candidates \
  --report <sanitized-evidence.json>
```

The wrapper removes its temporary schema and SQL user. These tests do not
certify a submitted license, perform business-human review, or establish a
hosted production deployment. No automatic extraction or autonomous approval
service is included.
