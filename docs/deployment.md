# Deployment

This guide applies to **SuperAgent Foundation**, based on LoopForge. See [current implementation status](superagent/implementation-status.md) for supported behavior and outstanding acceptance. Deploy this repository, not an upstream image that lacks these extensions.

## Delivery policy in this source build

`AGENTFLOW_DELIVERY_POLICY` defaults to `controlled_pr`. Configure the API and every Engine/Worker consistently. Unknown values stop startup. New Runs freeze the configured policy, and controlled parents remain controlled in child SOPs. Missing policies in persisted Runs/tasks are controlled, including when the deployment later selects `legacy`.

Controlled execution receives no platform Git token, credential-bearing HTTP clone URL, known hosting credential environment keys, or MCP server/file/call configuration. Model credentials and skills remain available. Runner publication actions, conversation pushes, automatic integration/merge and automatic Issue completion are disabled. Checkout also cannot initialize an empty remote or publish a missing integration branch. The old scheduled integration path fails closed. The explicit controller contribution path imports immutable accepted Git bundles, records fixed-SHA gates from independent SOP Runs and creates a draft GitHub PR. It is opt-in, requires attempt workspaces and platform administration, and grants no merge or business-completion capability. See [contribution delivery](contribution-delivery.md) for prerequisites and limits.

Set `AGENTFLOW_DELIVERY_POLICY=legacy` only when deliberately retaining the old publication/MCP behavior for new Runs. Legacy does not weaken a previously controlled or unstamped Run. Platform policy does not revoke previously issued credentials, ambient native-user Git/SSH access, or secrets embedded in user scripts/images; independent execution confinement remains necessary.

## Native development

`bash scripts/dev.sh` explicitly configures local secrets, starts only this checkout's MySQL service, builds the platform, provisions the `admin` account, installs the missing observability generated columns through an explicit script, applies `migrations/seed/*.sql`, and starts the server on port 8090. Existing catalog definitions are preserved by `INSERT IGNORE`. Keep `.env`, `.local/` and database volumes outside version control.

For a native installation, use Node.js 22+ on the API host and install the CLIs you choose. CodeBuddy is also used for requirement clarification; scheduled assistant tasks use whichever supported CLI the task selects:

```bash
npm install --global @tencent-ai/codebuddy-code
codebuddy --version
# Optional executors for Main Bot and SOP roles:
npm install --global @openai/codex@0.162.1 @anthropic-ai/claude-code@2.1.300
```

Ensure the service can find `codebuddy` and Node.js on its PATH, or set `AGENTFLOW_CODEBUDDY_BIN` to the installed executable. Configure the model key and its network environment in personal settings. The no-key script demo does not need the CLI. The production Docker image includes CodeBuddy and Codex. Claude Code is not redistributed: the first Main Bot conversation or SOP role that selects it installs the pinned `@anthropic-ai/claude-code` from npm into `$LOOPFORGE_DATA_DIR/cli`, so that first run waits for the download and later runs reuse it; Docker sandboxes and connected runners have their own CLI requirements.

Stop the foreground service with Ctrl-C. Start it again with the same command. MySQL data stays in its Compose volume. `docker compose stop mysql` stops the database without deleting it.

Use `AGENTFLOW_MYSQL_DSN` for your own MySQL deployment. A fresh account command or server connection creates missing schema objects. Apply existing-installation data migrations individually after reviewing their prerequisites; do not blindly replay all historical migrations against a new database.

## Container installation

```bash
git clone https://github.com/huasheng718/superagent-foundation.git
cd superagent-foundation
./start.sh             # build this checkout and start in the background
./start.sh logs        # follow platform logs
./start.sh stop        # stop; down removes containers but keeps data volumes
./start.sh restart     # restart the platform container
./start.sh pull        # rebuild source, or refresh the selected prebuilt image
./start.sh dev         # explicitly switch back to this source build
./start.sh release     # require an explicit LOOPFORGE_IMAGE; save image mode
./start.sh check       # read-only production configuration checks
./start.sh production  # check production configuration before build/start
```

The installer checks Docker, Compose, Python 3 and curl. It offers to install missing Python/curl packages using apt, dnf or yum on Linux (Homebrew on macOS), Docker using its official installer, and the missing Linux x86_64/arm64 Compose plugin using the pinned official release with SHA-256 verification. A plugin is installed for the active Docker user, or system-wide when Docker requires sudo. macOS Compose comes from Docker Desktop. Set `LOOPFORGE_YES=1` to accept installation prompts; internet access and package-manager/administrator permissions are still required. The installer generates `.env` with unique secrets, builds this source (or pulls the explicitly selected image) and starts the containers, installs the observability columns and applies `migrations/seed/*.sql`. It is safe to re-run. Set `AGENTFLOW_HTTP_PORT` or `LOOPFORGE_MYSQL_PORT` in the environment before the first run (or edit `.env` later) when the default ports are taken.

Run it as the installation owner, including an ordinary Linux user. If that user cannot access Docker directly, the installer validates sudo and uses `sudo -n docker` consistently for Compose, schema installation and seeds. Before each operation it checks whether sudo credentials expired during a long build, prompting again in a terminal or failing with instructions in unattended mode. It does not require adding the user to the Docker group or logging out. For unattended installations run `sudo -v` first; `LOOPFORGE_YES=1` accepts installation prompts but does not bypass operating-system permissions. Without sudo or Docker access it fails immediately with instructions. Rootless Docker uses its active context's Unix socket; remote Docker contexts need a separately configured deployment because sandbox workspaces are local bind mounts.

`./start.sh` and `bash scripts/install.sh` build this checkout as `superagent-platform:local` using `compose.yaml:compose.dev.yaml`. Platform and default sandboxes use the same image. The installer saves the Compose selection in `.env`, so plain Compose commands subsequently use the same stack. A new configuration also defaults to source mode. There is no automatic upstream LoopForge image fallback. An older `.env` without a saved Compose selection defaults to source mode even if it contains an old upstream image.

For a **tested image built from this repository**, explicitly select it:

```bash
LOOPFORGE_IMAGE=registry.example.com/team/superagent@sha256:<verified-digest> ./start.sh release
```

Replace the example with the actual image and digest; no image publication is claimed by these instructions. The installer saves the selected image and `COMPOSE_FILE=compose.yaml`. A later plain `./start.sh` keeps that selection. An explicit environment image overrides a previously saved one. `./start.sh dev` switches to building this checkout again. Release without an image fails before changing a source installation's selection. Tags are allowed for local testing; production preflight requires a digest. The base `compose.yaml` alone does not build: use the installer or explicitly layer `compose.dev.yaml` for source builds.

Pushing a `vX.Y.Z` tag on `main` runs the `release` workflow after successful `platform` CI on that exact commit. It publishes `ghcr.io/<owner>/<repository>` with version/commit tags, smoke-tests its digest using explicit image mode, and puts that digest in the release installation command. Set package visibility/access before distributing it. This workflow does not establish live production acceptance. Source updates rebuild the checkout; image updates require explicitly selecting the new digest. Back up data and review/apply required existing-installation SQL before starting the new version. Installer schema extensions/seeds are explicit installation steps; server startup does not apply data migrations.

Open the platform and create the first account in the browser; it receives platform administration. Until that account exists the login page only offers this setup step.

During initial setup, restrict the cloud security group or firewall to the operator's IP. Do not expose the first-account setup page to arbitrary Internet clients before the administrator account has been created.

New installations bind to `127.0.0.1:8090` and disable self-registration after first-administrator setup. Existing bind/signup settings are preserved; installations lacking these settings use the safe Compose defaults. To reach a new server initially, use an SSH tunnel, then create the first administrator. Do not expose bootstrap to arbitrary clients.

For production, terminate HTTPS at a reverse proxy on the same host, proxy WebSocket/streaming requests to `127.0.0.1:8090`, keep MySQL on loopback, and set these values in the existing `.env` without rotating its secrets:

```dotenv
LOOPFORGE_BIND=127.0.0.1
LOOPFORGE_PUBLIC_URL=https://agent.example.com
AGENTFLOW_ALLOW_SIGNUP=false
AGENTFLOW_SECURE_COOKIES=true
AGENTFLOW_DELIVERY_POLICY=controlled_pr
```

Run `./start.sh check` for configuration checks only, or `./start.sh production` to block build/start on an unsafe effective Compose configuration. The checks require authentication, closed registration, secure cookies, controlled delivery, loopback ports, non-placeholder secrets, owner-only `.env`, matching platform/sandbox images and a digest for prebuilt images. They never print the parsed secrets. They do not verify a running proxy/certificate, image provenance, GitHub permission scopes, independent hosts or hostile workload containment; use the [deployment acceptance checklist](operations/controlled-delivery.md). `./start.sh` remains the local HTTP development path. A LAN-only trusted-team experiment can explicitly set `LOOPFORGE_BIND=0.0.0.0`; that setting deliberately fails the production check.

`LOOPFORGE_PUBLIC_URL` records the operator-facing origin; it does not configure TLS or network routing. Configure `AGENTFLOW_PLATFORM_MCP_URL` separately for reachability from workers. Keep `AGENTFLOW_SESSION_SECRET` stable across API instances and restarts.

### Docker sandboxes

The platform exposes a built-in default sandbox (`local`) even when the catalog SQL has not been installed. It is an in-memory fallback and does not insert database rows. An existing saved `local` profile takes priority, including its configured driver and environment. The default cannot be deleted; edit its configuration or create another profile to change how tasks run. The full installer still imports starter SOPs and the welcome project.

For native services running as an ordinary host user, default/root Docker images run with that service user's UID/GID so private workspaces remain accessible without relaxing their permissions or adding capabilities. Images that explicitly select a non-root user retain that identity; operators must arrange compatible bind-mount ownership for those images.

The Compose deployment mounts the host Docker socket into the platform container so built-in AI SOPs run out of the box. `AGENTFLOW_ISOLATED_IMAGE` points to the installed platform image: default sandboxes start separate sibling containers from that same image, with no second CLI download or image build. They do not mount the Docker socket. Custom Dockerfiles still build and cache their own images. The run workspace is mounted at `/workspace` on the Compose network. Because bind mounts are resolved by the host Docker daemon, `LOOPFORGE_DATA_DIR` (written to `.env` by `scripts/configure.sh`, default `./.data`) is mounted into the platform container at the same absolute path; workspaces and CLI state live under it.

Access to the Docker socket is equivalent to root on the host. Two safeguards follow from that: the platform runs every task in a sandbox container, including SOPs configured for the `local` driver (`AGENTFLOW_LOCAL_SANDBOX=docker`), and only platform administrators can point a repository at a host source path. Even so, anyone who can run SOPs can execute code in containers on this host, so only give accounts to people you trust and do not expose the service to untrusted networks. To run without the socket, remove the `docker.sock` volume and the `AGENTFLOW_LOCAL_SANDBOX` setting from `compose.yaml`, then connect a separate Docker-capable worker or execution machine for AI SOPs; the script-only welcome SOP then runs inside the platform container.

### Slow networks: clone and image build limits

Cloning a repository and building a custom sandbox image report progress while they work, and their errors keep the last lines of git or Docker output. On the platform host the progress is recorded on the run. On an execution machine it is written to the runner's log, and only the final error reaches the platform. A step that prints nothing for its stall window is treated as hung and stopped. A step that keeps printing may be slow but is stopped only by a total limit. All settings take Go durations such as `90s` or `20m`; `0` turns a limit off. Each host reads its own environment, so set the clone limits on execution machines too.

| Setting | Default | Applies to |
|---|---|---|
| `AGENTFLOW_GIT_CLONE_STALL_TIMEOUT` | `5m` | `git clone` with no output. Not applied when the platform clones from a repository's local source path, whose final repack is silent. |
| `AGENTFLOW_GIT_CLONE_TIMEOUT` | off on the platform, `10m` on execution machines | Whole `git clone`. Execution machines keep their earlier fixed 10 minute limit as the default. |
| `AGENTFLOW_DOCKER_BUILD_STALL_TIMEOUT` | `10m` | `docker build` with no output. A quiet `RUN` step (a long compile) counts as no output. |
| `AGENTFLOW_DOCKER_BUILD_TIMEOUT` | off | Whole `docker build`. |

A clone that is stopped this way is not retried, since a retry would wait on the same network. When the execution host reaches GitHub or Debian mirrors slowly, point git at a proxy (`git config --global http.proxy`) and use a nearby package mirror in the Dockerfile rather than raising these limits.

## Accounts

The first account created in the browser becomes the platform administrator. After that, anyone who can reach the login page may sign up as a regular user when `AGENTFLOW_ALLOW_SIGNUP=true` is explicitly enabled. The installer and Compose default to `false`; the bare server binary retains its upstream default if this environment variable is omitted. Registered users can create requirements and run SOPs on your workers, so set `AGENTFLOW_ALLOW_SIGNUP=false` for any installation reachable from untrusted networks and create accounts explicitly instead.

Platform administrators create accounts under **Platform administration → Member accounts** (平台管理 → 成员账号): enter a login name and an initial password, optionally grant administration, and hand the password to the teammate. The same page lists every account and resets the password of a password account; accounts that sign in through GitHub or a gateway cannot be given a password there. New accounts join the welcome project, like self sign-ups.

LoopForge assumes a trusted team. Every signed-in account can list all projects, requirements and runs. Project settings and repositories can be changed only by the project owner or a platform administrator, and personal credentials only by their owner. Do not use one installation to separate untrusted tenants.

Without a browser, export the `.env` values for a native installation and run the explicit account command:

```bash
set -a; source .env; set +a
read -r -s -p 'Password: ' account_password; echo
printf '%s\n' "$account_password" | go run ./cmd/loopforge-account -user alice
unset account_password
```

Use `-admin` only when granting platform administration. Running the command again rotates that account's password. Password hashes use bcrypt. Browser sessions use signed HttpOnly, SameSite cookies and expire after seven days. No named person receives an implicit administrator role; additional administrators can be managed through the existing platform administration UI.

## Workers and runners

The upstream process roles remain `-role api`, `-role worker` and `-role all`. API and worker instances must share the MySQL database and configuration appropriate to their deployment. Set `AGENTFLOW_CONTROL_TOKEN`, `AGENTFLOW_CONTROL_URL` and `AGENTFLOW_WORKER_URL` when separating control and execution. Set `AGENTFLOW_PLATFORM_MCP_URL` to the actual platform URL reachable from execution containers.

Personal runners are connected from the execution-machine page with a short-lived binding code. The installer is served at `/runners/install.sh`. `AGENTFLOW_RUNNER_BINARY` can point to binaries with `{os}` and `{arch}` substitutions. Container images bundle Linux/macOS amd64/arm64 runner and gate binaries. Native source installations can build on demand with Go.

A platform administrator can share one CodeBuddy API Key under **Platform administration → Shared CodeBuddy Key** (平台共享 CodeBuddy Key): pick the network environment that issued it and the models members may use (the first is the default). Members without a key of their own then chat and run SOPs on it, limited to those models; a role configured with another model runs on the default and the run says so. Members with their own key keep using it with any model. Scheduled tasks, clarification, reports, bots and run evaluation still need the runner's own key. A scheduled task uses the key for its selected CLI and does not borrow the shared CodeBuddy key. The key is stored in the database like personal keys and is never shown again after saving. `AGENTFLOW_ASSISTANT_SPONSOR_LOGIN`, which lent one account's key limited to `deepseek-v4.1-flash`, still applies while no shared key is configured.

Configure provider credentials in the platform UI, not the host's global CLI configuration. Credentials are bound to the responsible actor by the upstream runtime. The home composer and SOP roles support CodeBuddy, Codex and Claude Code independently. Select the executor beside the home model selector; its API key (and optional Codex/Claude base URL) comes from your personal settings. Changing the executor preserves the draft; saved conversations record the executor used. Main Bot writes still pass through the platform tool gateway and its approval checks. Requirement clarification and notifications continue to use CodeBuddy. A scheduled assistant task can select any supported CLI and runs as an ordinary chat turn; Codex and Claude Code still need that account's own credential. Live model tests are opt-in; set `AGENTFLOW_LIVE_TESTS=1` only for the acceptance suites that need to load local credentials.

## Observability schema

After the base tables have been created, explicitly run `bash scripts/install-observability-schema.sh` for the Compose database. It installs absent `run_telemetry.adapter` and `run_logs.level` columns and their indexes from upstream migration `001_run_obs_generated_columns.sql`. The local development installer calls this step before installing templates. It can be repeated without reapplying existing columns. Application startup does not run data migrations.

For an external MySQL deployment, review that migration and check column existence before applying its corresponding statements. Stored generated columns can rebuild large existing tables; schedule the operation accordingly.

## Updating existing public SOPs

Repository binding is optional for all built-in generic SOPs. To update the original catalog instructions on an existing installation, explicitly apply:

```bash
docker compose exec -T mysql sh -c 'MYSQL_PWD="$MYSQL_PASSWORD" mysql -u loopforge loopforge' < migrations/016_public_optional_repositories.sql
```

This repeatable SQL changes only instruction fields that still match the original public seed. It preserves operator edits and project copies. New installations already receive these instructions from the public catalog seed; application startup does not migrate data.

## GitHub credentials

Users can choose **Connect GitHub** in personal credentials or while adding a repository, copy the displayed PIN, and approve it on GitHub. The platform saves the grant to that signed-in user's existing Git credential slot. The repository picker lists repositories owned by the account, shared with it, or accessible through its organizations, supports pagination and search over loaded repositories, and copies the selected repository's real default branch into the form. PAT entry remains available.

New installations use the built-in LoopForge OAuth App. Users do not create applications, configure a Client ID, or request a PAT: choose **Connect GitHub**, copy the PIN and approve the LoopForge application on GitHub. GitHub's confirmation cannot be bypassed; device authorization is not a zero-interaction login.

Application configuration is a deployment concern and is not shown in personal credentials. Operators who need their own application can register an OAuth App, enable **Device Flow**, keep token expiration disabled until refresh support is available, and set its public Client ID with `AGENTFLOW_GITHUB_CLIENT_ID` in `.env`. No client secret is needed. The deployment environment overrides the built-in application and any database override. The administrator-only `PUT /api/auth/github/config` endpoint remains available for an explicit database override; an empty Client ID explicitly disables device authorization rather than silently restoring the default. Existing explicit configurations are preserved.

The flow requests the OAuth `repo` scope so private cloning, pushes, Issues and PR operations work. GitHub's OAuth scope covers accessible repositories; choosing a repository in LoopForge binds it to the project and does not narrow the OAuth grant. Organization SSO or third-party-app restrictions may require organization approval. See [GitHub's device authorization documentation](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps#device-flow). Pending grants expire and are held only in API memory: use session affinity for `/api/auth/github/*` behind multiple API instances, and restart authorization after an API restart. Tokens and GitHub device codes are never sent to the browser.

GitHub Issue import and PR status synchronization use the project owner's Git token. Git operations and the optional GitHub MCP integration use the responsible user's token. Public reads may work without a token; private repositories and writes require repository access. GitHub MCP must be installed explicitly and sends credentials only to its official HTTPS endpoint.

Git commits created by the platform use the responsible local account name and a non-deliverable `@users.noreply.loopforge.invalid` email. A local account does not establish a corporate email or a verified GitHub identity.

## CodeBuddy authentication environment

In personal settings, choose the network environment belonging to the API key; the "get API key" link opens that edition's key page. A key from one edition returns HTTP 401 when sent to another edition, and keys from third-party model providers (DeepSeek, OpenAI and similar) are not CodeBuddy keys. The setting is per user; public installations do not assume an enterprise environment.

| Edition | Stored value | Key page |
| --- | --- | --- |
| International | empty | https://www.codebuddy.ai/profile/keys |
| China | `internal` | https://copilot.tencent.com/profile/ |
| iOA | `ioa` | https://tencent.sso.copilot.tencent.com/profile/keys |

`PUT /api/users/:id/credentials` accepts only these values (`public` and `China` are also understood) and rejects anything else with HTTP 400, because the CLI silently falls back to the international edition for values it does not recognize.

Other dedicated/self-hosted configurations are described in the [official CodeBuddy identity documentation](https://www.codebuddy.ai/docs/cli/iam). Configure the model catalog with `AGENTFLOW_ASSISTANT_MODEL` and `AGENTFLOW_ASSISTANT_MODELS`; ordinary roles can select a model independently.

## Isolated container acceptance

Run `python3 scripts/test-install.py -v` for installer regression tests covering sudo selection, direct Docker access, unattended permission failures, repeated installation, image mode and access URLs. With a locally built platform image, `LOOPFORGE_INSTALL_TEST_IMAGE=your-image bash scripts/test-install-live.sh` verifies real root and UID 65534 installations, non-loopback HTTP access, first-account creation and repeated installation in isolated Compose projects on random ports. This live test requires a Linux root test runner to drop privileges; it removes its own containers and volumes afterwards. To also exercise real sudo inside a disposable UID 1000 installer container, build `scripts/testdata/install-sudo.Dockerfile` with `--build-arg LOOPFORGE_TEST_BASE=your-image`, then pass that test image as `LOOPFORGE_INSTALL_SUDO_TEST_IMAGE`. The production installer itself does not require root.

From the repository root, run `bash scripts/test-container.sh` after installing `e2e/` dependencies and Chromium. It builds the production image, creates an isolated Compose project and database on ports 8093/33317, provisions test accounts, explicitly applies the observability baseline and public catalog, checks bundled CLIs and cross-platform runner/gate binaries, and runs the public browser suite. Its containers and volumes are removed afterwards. Override `LOOPFORGE_CONTAINER_TEST_PORT` and `LOOPFORGE_CONTAINER_MYSQL_PORT` when those ports are in use. Model calls are disabled for this clean-install check.
