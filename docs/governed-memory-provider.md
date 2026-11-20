# Private governed memory index

The optional TencentDB adapter indexes already approved native memories for L0/BM25 ranking. `MemoryRepo` remains the authority for content, project, approval, revision, expiry and revocation. It does not extract knowledge, approve suggestions, change TeamAI releases or introduce a scheduler.

## Deployment contract

The default mode uses existing native recall. To enable the adapter, an operator supplies server environment variables:

- `AGENTFLOW_AUTH_REQUIRED=true`
- `AGENTFLOW_MEMORY_PROVIDER_URL`: a private HTTPS origin or loopback HTTP origin, without userinfo, query, fragment or path.
- `AGENTFLOW_MEMORY_PROVIDER_TOKEN`: server-held provider service Bearer.
- `AGENTFLOW_MEMORY_PROVIDER_INSTANCE`: the deployment owner's persistent instance label.
- `AGENTFLOW_MEMORY_PROVIDER_GENERATION`: a new label whenever the owned provider data directory/index is replaced.
- `AGENTFLOW_MEMORY_PROVIDER_SERVICE`: the provider service ID; default `default`.

Keep the provider private. Its service Bearer is not user/project authorization. Browsers and Workers receive no provider credentials or writable provider endpoint. Scope comes from the current authenticated human, authorized project and the platform's project-UI identity; caller `x-tdai-*` headers and body scope fields are rejected. Direct Worker/MCP recall is not introduced by this adapter.

The checked protocol is TencentDB source `42d4cde50c5a83182fced8c584f9ddb32a620615`, package `1.0.2-beta.1`, observed health `0.1.0`, SQLite FTS schema v2. The instance/generation labels are operator configuration, not remotely attested database identities. Run a dedicated extraction/embedding/warmup/telemetry-disabled deployment with the upstream MIT license preserved. No upstream source or installer is copied into this product.

Apply [migration 038](../migrations/038_governed_memory_provider.sql) manually to an existing MySQL deployment after review. The two new tables hold derived mirror metadata and immutable recall receipts. No startup seed, backfill or data migration runs.

## API

`POST /api/projects/:id/memory-provider/sync` accepts `{ "limit": 10 }` (optional, 1–10). It synchronizes only the current human's scope and returns `eligible`, `ready`, `pending`, `failed`, `cleanupPending` and `complete`. Repeated explicit requests advance bounded work; there is no background synchronization. A request handles at most ten memory/cleanup operations, 256 KiB of mirror text, and three seconds of provider work. Each HTTP call has a one-second deadline, 256 KiB request cap and 1 MiB response cap; redirects are disabled.

`POST /api/projects/:id/memory-recall` retains `{ "query": "rollback", "limit": 5 }` and its native memory-array response. Provider mode requires the entire bounded eligible set to have ready mirrors and verifies actual indexed records before searching. It fails closed with `memory_provider_sync_required`, `memory_provider_unavailable`, `memory_provider_integrity` or `memory_provider_capacity`; it never switches to native recall on a provider failure.

With `includeReceipt: true`, provider mode returns `{ "items": [...], "receipt": {...} }`. Native mode does not manufacture provider receipts. Receipts contain server-owned scope, deployment binding, health observation, query/eligible-set digests, native revision/content pins, actual provider record IDs/versions, scores, request IDs and latency. Provider text is never returned as native memory content.

The supported eligible index is at most 500 approved records and 8 MiB of title/content per project. The mirror ledger is bounded to 2,000 rows per configured caller scope/binding, including retained cleanup history. Overflow is explicit. Recall returns at most ten memories and 64 KiB. Each native memory is split at UTF-8 boundaries into at most four 7,000-byte chunks. Coverage queries page the complete bounded scope; a persisted ready ledger alone is not accepted as evidence that a replaced data directory still exists.

## Unknown writes and revocation

Upstream add generates random IDs and has no client idempotency key. A native SQL transaction reserves one durable add intent while checking the approved native pin. Only its creator may append once. Existing pending intents perform exact-session discovery; an empty query never permits another add. Complete matching results can recover real record IDs after a lost acknowledgement. SQL locks are not held across provider HTTP calls.

Native revocation succeeds independently of provider deletion. Search results are mapped through the current scope's ledger and checked against the native state/revision/digest/expiry again. Synchronization deletes only exact owned sessions and verifies their absence. A revoked uncertain add retains `delete_pending` even after an empty query, because the original add could arrive later; retiring the owned provider generation is the physical cleanup boundary.

Upstream standalone add also appends conversation JSONL. Its delete endpoint removes database/FTS records, not that JSONL copy. `deleted_count` and empty queries do not prove physical erasure, log/backup retention or SSD secure deletion. The deployment owner must define those independently.

## Frozen execution context and Runner compatibility

The caller may select `memoryRecallReceiptId` alongside `releaseId`/`memoryIds` in context resolution and a Run's `teamContext`. The server verifies current user/project, receipt digest, ten-minute selection TTL and exact selected native pins. It copies the receipt into the immutable context and its digest. Retry/replay uses the original receipt and does not contact the provider or select latest. The selection TTL does not expire an already frozen Run; native memory expiry/revocation is still enforced at execution and Runner claim.

Receipt metadata is carried in the existing Runner governance binding without credentials. **Receipt-bearing assignments require Runner 0.13.0 or newer**, checked before queuing and again at claim. Normal governed assignments without a receipt continue to support 0.12.0. Older decoders must not receive receipt assignments because dropping the new field changes their assignment digest. Optional fields remain omitted for legacy Runs.

## Verification and limits

`e2e/api/47-memory-scope-storage.test.mjs` passed against an isolated local MySQL/API/private-provider installation: one test, zero skips, 12.774s and 140 API operations. Authentication was required, local login and trusted identity headers were disabled, and Alice/Bob were ordinary project owners with independent synthetic tokens. The actual APIs rejected cross-project governance access, forged scope, and another user's receipt even after both users could legitimately read the same project. Candidate/rejected memories stayed out of recall; approved pins matched native content/revision/digest; warmed recall respected membership removal, revocation and a real ten-second native expiry. Public project directory metadata remains intentionally visible to authenticated users.

A separate actual platform restart retained exact native/mirror/candidate payload hashes and the original receipt/context; explicit synchronization added no conversation JSONL bytes. After the owned provider was stopped and its directory removed, live recall returned `503 memory_provider_unavailable` without native fallback; resolving the already recorded exact context still succeeded. These checks started no Runner, model or CLI. They do not establish wall-clock receipt-TTL expiry, a live same-binding index-directory replacement, external TLS or physical host isolation.

Provider logical data-file sizes were 930,936 bytes when empty, 1,391,980 with the two owner mirrors, and 1,702,166 after additional member/expiry probes and Alice mirror deletion while Bob remained. JSONL stayed at 2,422 bytes after logical deletion. SQL JSON payload sizes were measured separately; audit/receipt rows were retained. These tiny-fixture file sizes include SQLite WAL/SHM and metadata, are not per-memory marginal pricing, and do not prove physical erasure. The owned platform/schema/account/token configuration and provider generation were removed; the main local installation retained native recall by default.

Portable unit/HTTP tests cover uncertain and late add discovery, no duplicate add after restart, isolation, forged scope, corrupted/unknown records, missing index, approval changes during search, failed deletion, bounded transport, index overflow, receipt ownership/TTL/digest and Runner version gates. The opt-in `TestMemoryProviderLocalRealHTTPReceiptAndUncertainAdd` uses an operator-owned loopback connection file and a fresh disposable provider; it never installs or starts the provider. `scripts/test-memory-provider-mysql.py` applies SQL038 to a new owned schema, verifies two independent pools and fresh-process reads, and removes only its generated schema/account.

The upstream FTS implementation can swallow internal search errors into an empty result and can lose recall quality when global overretrieval is filtered by scope. Health checks reject known missing FTS, but an HTTP 200 empty result is not proof of an error-free internal search. This adapter establishes governed transport/ranking and fixed receipt provenance. It does not certify real model extraction quality, generated-skill promotion, token/fee utility, long-term retention or production deployment isolation.
