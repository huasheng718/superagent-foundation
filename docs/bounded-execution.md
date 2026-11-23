# Bounded execution and repair

Bounded execution is an opt-in extension of the existing Engine and Run ledger.
It uses the existing Runner protocol and never starts a second scheduler.
No monetary allowance, currency conversion or provider price is inferred.

## Policy

A normal `POST /api/runs` or Issue dispatch request may include:

```json
{
  "executionBudget": {
    "mode": "metered",
    "maxTotalTokens": 100000,
    "maxCostMicrosUSD": 1000000,
    "currency": "USD",
    "priceVersion": "operator-reviewed-2026-11",
    "maxInvocations": 5,
    "maxNoProgress": 2
  }
}
```

These are example limits, not defaults. The example permits one USD of observed
provider-reported cost. Omit a zero-valued token/cost cap to disable that cap.
`maxInvocations` is required (1–1000); an enabled no-progress threshold is 2–100.
At least a token cap, cost cap or no-progress threshold is required. A cost cap
requires `USD` and an explicit `priceVersion` audit label. The label records the
operator's pricing basis; it does not certify a tariff or turn an estimate into
provider-reported billing.

The policy is copied and hashed before Run admission. Existing Runs cannot
acquire a policy later, and an admitted policy cannot be changed or removed.
`Run.executionBudget` exposes its policy digest, consumed counters, invocation
receipts, session watermarks, active reservations, progress and sticky stop reason.
Legacy Runs without a policy keep their existing behavior.

## Actual support and hard limits

| Execution path | `strict` token/cost cap | `metered` token/cost cap | No-progress-only policy |
| --- | --- | --- | --- |
| Existing Agent CLI adapters | Rejected before starting the Run | Observes trusted adapter usage; unknown usage stops work | Supported for bounded Agent repair |
| Script/command node | Rejected before starting the Run | Rejected: opaque shell has no trusted usage ledger | Admitted; this does not add an Agent repair loop to a script |
| Approval-only workflow | Admitted; no provider is invoked | Admitted; no provider is invoked | Admitted; no repair is performed |
| Child SOP or a workflow dispatching one | Rejected | Rejected | Rejected until shared parent reservations exist |

**Current adapters cannot enforce a hard provider bill or token ceiling.**
`strict` requires a provider-enforced reservation, which no current CLI/shell
adapter implements. Admission therefore rejects executable workflows carrying a
strict token/cost cap, including a command that might launch a model itself.
It does so before earlier nodes or marker commands execute.

`metered` is explicitly observed-usage control. It cancels current Engine node
executions and prevents further work when a reported counter reaches its cap.
A CLI can incur additional usage before the next report or before cancellation
takes effect. Hooks, verification commands, arbitrary external tools and activity
outside trusted adapter usage do not constitute independently metered provider
billing. This mode is not a claim that all possible external expenditure is
bounded. Script/command workflow nodes carrying a token/cost cap are rejected
rather than treated as zero cost.

## Durable accounting

The Engine reserves each Agent invocation before provider execution, including
implicit session-recovery and missing-submit retries. Reservations are serialized
against current node attempt and Run ownership; the repository revision fence
rejects stale Run snapshots instead of overwriting consumed allowance.

Usage is cumulative per node/provider session. Input/output high-watermarks and
provider total tokens are reconciled conservatively; duplicate/lower snapshots
never refund usage, and total-only snapshots are not counted twice when detailed
fields arrive. USD cost is rounded upward to integer micro-USD. Only explicitly
`reported` cost is eligible for a monetary cap; estimated or unknown cost stops
a cost-budgeted execution. Hooks, legacy hooks and OTel side channels cannot
supply ledger usage. One invocation cannot silently switch accounting sessions.
Invalid counters, overflows, missing session identity or missing usage fail closed.

The ledger remains on the Run through retries and durable reloads. A previous
unfinished/replayed invocation is uncertain and cannot receive a fresh allowance.
Manual retry cannot reset spend or clear a budget/no-progress stop. A new Run
requires a new explicit policy authorization. Revision conflicts preserve pending
usage and retry budget writes, so a Runner/API update cannot silently discard a
pending accounting snapshot. Prepared contribution integrations and subsequent
controlled delivery recheck both controller and source Run budget state.

## No-progress repair

Enable the existing Agent Loop and give its failed verification gate
`onFailure: "loop"`. The policy bounds that existing loop; `maxInvocations`
also bounds implicit additional Agent invocations. The policy does not invent
another loop or override the Agent's smaller iteration limit.

On a failed repair iteration the controller hashes the complete sorted repository
identity/product-tree set. Local snapshots use a private Git index, preserve HEAD
and the user's staging, and exclude control reports/credentials using the existing
contribution exclusion policy. Remote snapshots require controller-accepted,
independently reconstructed contribution bundles for the current node attempt.
Missing or incomplete repository evidence stops with
`progress_evidence_unavailable`.

Only a real product-tree change resets the consecutive failure count. A changed
verifier exit status, random diagnostic text, filename inventory, Agent claim,
control report or empty Git commit cannot reset it. Diagnostics remain in the
normal gate events and node error for audit. Reaching the configured consecutive
unchanged-tree threshold stops with `no_progress`. This detects unchanged content;
it does not prove that every changed product file improves the implementation.

## Deployment and evidence

For an existing deployment, apply `migrations/034_run_execution_budget.sql`
manually with the normal deployment process before enabling this feature. It
adds nullable `runs.execution_budget`; no production seed or data backfill runs.
The normalized header read/write mapping persists the entire ledger. SQL 035
separately owns the governed intelligence column and its tables.

Verified locally on 2026-11-17:

- Domain tests cover policy/strict rejection, monotonic session totals, integer
  cost rounding, unknown usage, overflow, sticky stops, invocation replay and
  no-progress thresholds.
- Git tests cover real content changes, deleted/staged-deleted files, control
  noise, empty commits and preservation of user staging/HEAD.
- Repository tests reopen a file-backed SQLite connection and verify durable
  spend/receipts, immutable policy and stale/rollback rejection.
- Engine tests cover concurrent reservations, usage authority, conflict recovery,
  opaque-shell admission rejection, a real Git checkout with a mock-provider
  failing-gate loop stopping after two unchanged trees, alternating verifier
  errors, retry denial and stopped/uncertain contribution rejection.
- All Engine tests passed with `go test ./internal/engine -parallel 4 -count=1`.
  The first unrestricted parallel run had 13 older mock/lease/approval tests
  time out; the bounded-concurrency full rerun passed. Latest focused regressions
  passed after the final budget/tree/approval changes.

These are local code/SQLite/Git/mock-provider checks, not a paid-provider hard
reservation experiment, a process-kill billing audit or a new production rollout.
No provider-backed strict hard-budget acceptance is claimed.

### Disposable MySQL and process recovery acceptance

The optional acceptance runner uses a dedicated random schema and temporary SQL
user in an explicitly disposable `superagent-p0-*` MySQL container. It does not
read the application datasource:

```sh
python3 scripts/test-execution-budget-mysql.py --container superagent-p0-contribution
```

The runner requires an already running official MySQL container with a single
loopback-only published database port. It creates/removes only its private
`budget_acceptance_*` schema and user. An optional `--report PATH` writes sanitized
test output and cleanup status; credentials are passed only through environment.

On 2026-11-17 this acceptance passed against real MySQL 8.0.45: two Store pools
competed at one revision with exactly one reservation winner, stale/rollback
writers were rejected, and new OS test processes retained cumulative spend,
rejected unfinished invocation replay and enforced sticky cap state. Reported
usage was synthetic; this verifies database concurrency/process recovery without
claiming a paid-provider hard budget or killed live Engine reconciliation.

Provider audit retained strict rejection. Installed Codex, OpenCode and DeepSeek
Harness do not have a verified per-Run provider reservation integrated with this
platform. Codex rollout/goal limits, OpenCode steps/output limits and DSH token
metering do not establish that contract. Claude's documented
[`--max-budget-usd`](https://code.claude.com/docs/en/cli-reference) is checked
against client estimates after response costs arrive and can overshoot; it is
not a strict financial ceiling. No claim that every upstream provider feature
was exhaustively proven absent is made: unsupported or unverified reservation
capabilities receive no strict authority.
