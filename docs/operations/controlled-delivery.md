# Controlled delivery deployment acceptance

Controlled execution creates a draft PR only. It never merges, enables auto-merge or equates execution success with business acceptance. Hosting must enforce review independently of platform prompts and API permissions.

## Production configuration gate

Deploy the SuperAgent Foundation repository or its explicitly verified image. `./start.sh` builds this checkout by default; `./start.sh release` requires an explicit image. No published upstream LoopForge image is selected automatically. Configure HTTPS and keep API/MySQL ports on loopback as described in [deployment](../deployment.md).

Run `./start.sh check` to inspect effective Compose configuration without starting services. Use `./start.sh production` to enforce the same checks before build/start. A failed check exits nonzero. The gate checks authentication, disabled registration, secure cookies, controlled delivery, loopback ports, secret placeholders/length, owner-only configuration, matching platform/sandbox image and a digest for prebuilt images. Passing means configuration prerequisites are satisfied; it does not prove a live proxy, source/image provenance or external authorization.

## Acceptance record

Record the exact source commit (and any patch), image ID/digest, Runner versions and dated results without secrets. Each target repository needs its own hosting-policy and publisher-identity evidence.

| Gate | Required evidence | Current boundary |
| --- | --- | --- |
| Source/image identity | Build this checkout; record SHA and local diff, or verify an image's release revision and digest; platform and sandbox match | Default/source selection and effective Compose configuration tested locally; no new production image built or published |
| HTTPS and accounts | Valid deployed certificate, authenticated browser login, streaming/WebSocket proxy, first administrator set up over a restricted path, subsequent registration denied | Configuration and bootstrap/registration regression only; live server proxy not accepted |
| Human review | Run the read-only policy checker against each actual target and retain its report | Seven source-repository policy checks passed on 2026-11-19; repeat for the deployed target and date |
| Least-privilege publisher | Record selected installation/repositories and permissions; test allowed draft creation plus denied default writes, force-push, bypass and unreviewed merge in a disposable target | Pending; no dedicated identity supplied. Broad operator credentials do not qualify |
| Independent Worker | Use a separate Linux host to test TLS/network loss, recovery, stale-attempt rejection and cleanup | Local Mac/Docker Linux passed; physical host separation pending |
| Hostile execution limits | Verify memory/CPU/PID/disk/network/process-group enforcement and cleanup on the actual driver/kernel | Local storage quota refused on overlay2/extfs; fail-closed behavior passed, normal hardening/enforcement not accepted |
| Recovery and retention | Restore a database/artifact backup, manually apply reviewed SQL, test restart, reconcile uncertain publication without another POST, verify retention/deletion including provider backups | Local recovery evidence exists; production restore, retention and physical deletion pending |

The default Docker-socket deployment is for a trusted team. It grants the controller host-daemon authority and is not hostile tenant isolation. Do not turn a failed storage-limit check into a pass by removing limits. The opt-in `AGENTFLOW_TEST_DOCKER_HARDENED=1 go test ./internal/runtime -run '^TestDockerHardenedLocalCompatibility$' -count=1 -v` distinguishes daemon refusal from normal startup; even normal startup/resource inspection alone does not prove hostile containment.

## Hosting policy

Run `python3 scripts/check-delivery-policy.py owner/repository` with an authenticated GitHub CLI. It reads the actual default branch, effective branch rules and contributing repository rulesets. Acceptance requires one or more approvals, stale-review dismissal, a different approver after the last push, resolved discussions, no force-push/deletion, active rules and no bypass actors. The script does not modify hosting settings.

A ruleset matching `~DEFAULT_BRANCH` should apply these rules with `enforcement: active` and an empty `bypass_actors` list. Changes to approval requirements are an operator action, separate from publication. Protect the repository that receives contributions; protecting only the platform source repository does not protect other project targets. The PR author cannot supply their own required approval. Conversion from draft and human approval remain explicit human actions.

## Publisher identity

Use a repository-selected GitHub App installation or fine-grained token. Give only metadata read, contents read/write for create-only integration branch/object publication and pull requests read/write for draft PR creation. Do not give administration, Actions/workflows, organization administration or rule bypass rights. GitHub contents/PR write permissions alone are not a create-only transport: effective branch rules must prevent default-branch writes and unreviewed merging.

Keep publishing credentials only in the controller's protected user/provider configuration. Workers receive neither hosting credentials nor memory/configuration publication rights. A personal Worker can still hold ambient host credentials; use a clean execution account and an appropriate containment profile.

`--require-least-privilege` deliberately exits unsuccessfully because a read-only ruleset inspection cannot prove the deployed token's scope. Operators must record the constrained installation/repository selection and effective permissions, and test denied default-branch writes, force-push, bypass and unreviewed merge in a disposable repository. A broad existing `gh` operator session must never be reported as that constrained identity. Do not place secrets in the evidence or shell arguments.

## Database and runtime

Existing installations review and manually apply numbered SQL migrations. There is no server-startup data migration or seed; running the installer explicitly imports schema extensions and starter catalog SQL. Build the chatbot (`bash scripts/build-chatbot.sh`) before building the Go server, which embeds `web/native-chat`. Deploy current platform and compatible Runner binaries together. Node placement requires Runner 0.11.0+ and attempt-isolated repositories with explicit base commits. Governed release/memory execution additionally requires Runner 0.12.0+, and provider-receipt assignments require 0.13.0+; old Runners cannot claim those tasks.

Local Mac/Docker Linux evidence proves cross-OS execution on one physical host. Independent cloud-host network/TLS/recovery, production artifact retention, and hostile-code containment require deployment-specific acceptance.
