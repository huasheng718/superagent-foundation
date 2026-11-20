package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"agentflow/internal/domain"
)

func TestDockerImageUserDefaultAndExplicitOwner(t *testing.T) {
	for _, user := range []string{"", "root", "0", "runner:runner", "1001:1002"} {
		t.Run("user="+user, func(t *testing.T) {
			config := map[string]any{}
			if user != "" {
				config["User"] = user
			}
			var rendered bytes.Buffer
			tmpl := template.Must(template.New("image-user").Option("missingkey=error").Parse(dockerImageUserTemplate))
			if err := tmpl.Execute(&rendered, map[string]any{"Config": config}); err != nil || rendered.String() != user {
				t.Fatalf("image user=%q err=%v", rendered.String(), err)
			}
			if os.Geteuid() == 0 {
				return // Native owner remapping only applies to non-root services.
			}
			workspace := t.TempDir()
			cvRInstallFakeDocker(t, workspace)
			createLog := filepath.Join(t.TempDir(), "create-args")
			t.Setenv("CVR_DOCKER_IMAGE_USER", rendered.String())
			t.Setenv("CVR_DOCKER_CREATE_LOG", createLog)
			t.Setenv(dockerHardenedEnv, "")
			defer os.RemoveAll(dockerIsolationMaskRoot(workspace))
			sb := &dockerSandbox{id: "image-owner", name: "image-owner", workspace: workspace, dockerfile: domain.DockerfileDefinition{Content: "FROM scratch\n"}}
			if err := sb.ensureStarted(context.Background()); err != nil {
				t.Fatal(err)
			}
			args, err := os.ReadFile(createLog)
			if err != nil {
				t.Fatal(err)
			}
			wantOverride := user == "" || user == "root" || user == "0"
			hasOverride := strings.Contains(string(args), "\n--user\n")
			if hasOverride != wantOverride || (wantOverride && !strings.Contains(string(args), fmt.Sprintf("\n%d:%d\n", os.Geteuid(), os.Getegid()))) {
				t.Fatalf("image user %q create args: %s", user, args)
			}
		})
	}
}

func TestDockerSandboxLifecycleWithFakeCLI(t *testing.T) {
	workspace := t.TempDir()
	cvRInstallFakeDocker(t, workspace)
	_ = os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("hidden"), 0o600)
	dockerfile := domain.DockerfileDefinition{ID: "cvR", Content: "FROM scratch\n", Files: map[string]string{"asset.txt": "asset"}}
	sbAny, err := createDocker(context.Background(), "cvR-run", workspace,
		domain.SandboxProfile{Driver: "docker", ReadOnlyRootFS: true, Environment: map[string]string{"A": "1"}},
		SandboxRequirements{BunFS: true}, dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	sb := sbAny.(*dockerSandbox)
	if sb.ID() != "cvR-run" || sb.Workspace() != workspace || sb.Driver() != "docker" {
		t.Fatalf("sandbox=%+v", sb)
	}
	out, err := sb.Exec(context.Background(), CommandSpec{Argv: []string{"sh", "-lc", "echo ok"}, environment: map[string]string{"B": "2"}}, nil)
	if err != nil || !strings.Contains(out, "/workspace") {
		t.Fatalf("exec=%q err=%v", out, err)
	}
	if !sb.started || sb.builtImage == "" {
		t.Fatalf("sandbox did not start: %+v", sb)
	}
	if err := sb.BindWorkspace(context.Background()); err != nil {
		t.Fatal(err)
	}
	sb.invalidate()
	if sb.started {
		t.Fatal("invalidate retained started")
	}
	if err := sb.Recreate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sb.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDockerDiscoveryImagesAndBuildHelpers(t *testing.T) {
	workspace := t.TempDir()
	cvRInstallFakeDocker(t, workspace)
	ctx := context.Background()
	source, err := dockerWorkspaceBindSource(ctx, "box")
	if err != nil || source != workspace || !dockerContainerReusable(source, nil, workspace) {
		t.Fatalf("source=%q err=%v", source, err)
	}
	if dockerContainerReusable("", nil, workspace) || dockerBindMatchesWorkspace("", workspace) {
		t.Fatal("empty bind matched")
	}
	found, err := dockerSandboxesUnder(ctx, filepath.Dir(workspace))
	if err != nil || found["agentflow-cvr"] != filepath.Base(workspace) {
		t.Fatalf("found=%v err=%v", found, err)
	}
	mounts, err := collectDockerSandboxMounts(ctx, []string{"id1"})
	if err != nil || !strings.Contains(mounts, "agentflow-cvr") {
		t.Fatalf("mounts=%q err=%v", mounts, err)
	}
	if !dockerObjectGone("No such container", os.ErrNotExist) || dockerObjectGone("", nil) {
		t.Fatal("docker gone classification")
	}
	listed, err := listCachedDockerImages(ctx)
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed=%v err=%v", listed, err)
	}
	inUse, err := cachedDockerImagesInUse(ctx)
	if err != nil || len(inUse) != 1 {
		t.Fatalf("inUse=%v err=%v", inUse, err)
	}
	removed, err := reclaimCachedDockerImages(ctx, map[string]struct{}{})
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if got := staleCachedDockerImages([]string{"agentflow-img-a", "other"}, map[string]struct{}{}, map[string]struct{}{}); len(got) != 1 {
		t.Fatalf("stale=%v", got)
	}
	if normalizeCachedDockerImageName("agentflow-img-a:latest") != "agentflow-img-a" || normalizeCachedDockerImageName("ubuntu") != "" {
		t.Fatal("image normalization")
	}
	dockerfile := domain.DockerfileDefinition{ID: "../bad", Content: "FROM scratch", Files: map[string]string{"Dockerfile": "ignored", "sub/a": "a"}}
	dir, err := prepareDockerfileBuildContext(workspace, dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "a")); err != nil {
		t.Fatal(err)
	}
	image, err := ensureDockerImage(ctx, workspace, dockerfile)
	if err != nil || !strings.HasPrefix(image, dockerCachedImagePrefix) {
		t.Fatalf("image=%q err=%v", image, err)
	}
	if _, err := createDocker(ctx, "x", workspace, domain.SandboxProfile{}, SandboxRequirements{}, domain.DockerfileDefinition{}); err == nil {
		t.Fatal("empty dockerfile accepted")
	}
}

func TestDockerArgumentAndErrorBranches(t *testing.T) {
	if got := dockerWorkspaceExecArgv(nil); !strings.Contains(strings.Join(got, " "), "pwd") {
		t.Fatalf("argv=%v", got)
	}
	got := dockerWorkspaceExecArgv([]string{"bash", "-lc", "echo x"})
	if len(got) < 3 || got[len(got)-2] != "-c" || !cvRContains(got, "-c") {
		t.Fatalf("login flag retained: %v", got)
	}
	if out := dropShellLoginFlag([]string{"tool", "-lc"}); out[1] != "-lc" {
		t.Fatalf("non-shell changed: %v", out)
	}
	for _, text := range []string{"No such exec instance", "No such container", "container not found", "Error response from daemon: box is not running"} {
		err := os.ErrNotExist
		if !looksLikeDockerExecGone(text) || !isMissingDockerExec(cvRErrorsJoin(err, text)) || !IsSandboxInterrupt(cvRErrorsJoin(err, text)) {
			t.Fatalf("not classified: %s", text)
		}
	}
	if !IsSandboxInterrupt(context.Canceled) || IsSandboxInterrupt(nil) {
		t.Fatal("interrupt classification")
	}
	if !IsCwdMountFailure(cvRErrorsJoin(os.ErrInvalid, "exit status 126")) || isDockerCwdUnsafe("", nil) {
		t.Fatal("cwd classification")
	}
	line := "Error response from daemon: No such container: box"
	if excerpt := dockerInterruptExcerpt("noise\n" + line + "\n"); excerpt != line {
		t.Fatalf("excerpt=%q", excerpt)
	}
	if annotateDockerExecError(line, os.ErrInvalid) == os.ErrInvalid {
		t.Fatal("docker error was not annotated")
	}
	if !ephemeralDockerImage(tempDockerImageName("x")) || ephemeralDockerImage(dockerCachedImagePrefix+"x") {
		t.Fatal("ephemeral image classification")
	}
}

func TestHardenedDockerResourceLimitsAreExplicitAndFailClosed(t *testing.T) {
	if limits, err := dockerResourceLimitsFromEnv(); err != nil || limits.memory != "" {
		t.Fatalf("legacy Docker path unexpectedly hardened: %+v err=%v", limits, err)
	}
	t.Setenv(dockerHardenedEnv, "1")
	limits, err := dockerResourceLimitsFromEnv()
	if err != nil || limits.memory != dockerDefaultMemory || limits.cpus != dockerDefaultCPU || limits.pids != dockerDefaultPIDs || limits.storage != dockerDefaultStorage || limits.tmpfs != dockerDefaultTmpfs {
		t.Fatalf("default hardened limits=%+v err=%v", limits, err)
	}
	args := appendDockerResourceLimitArgs([]string{"create"}, limits)
	text := strings.Join(args, " ")
	for _, needle := range []string{"--memory 4g", "--cpus 2", "--pids-limit 512", "--storage-opt size=8g"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("hardened Docker args missing %q: %v", needle, args)
		}
	}
	t.Setenv(dockerPIDsLimitEnv, "0")
	if _, err := dockerResourceLimitsFromEnv(); err == nil {
		t.Fatal("invalid PID limit did not fail closed")
	}
	t.Setenv(dockerPIDsLimitEnv, "512")
	t.Setenv(dockerMemoryLimitEnv, "not-a-size")
	if _, err := dockerResourceLimitsFromEnv(); err == nil {
		t.Fatal("invalid memory limit did not fail closed")
	}
}

func TestHardenedSandboxRejectsNativeFallback(t *testing.T) {
	t.Setenv(dockerHardenedEnv, "1")
	m := NewManager(t.TempDir())
	if _, err := m.AcquireRun(context.Background(), "hardened-native", domain.SandboxProfile{Driver: "local"}, SandboxRequirements{}, RunCredentials{}, SandboxResources{}); err == nil || !strings.Contains(err.Error(), "requires the docker driver") {
		t.Fatalf("hardened native fallback accepted: %v", err)
	}
}

func TestDockerMasksAndManagerPaths(t *testing.T) {
	workspace := t.TempDir()
	cvRInstallFakeDocker(t, workspace)
	_ = os.WriteFile(filepath.Join(workspace, "CLAUDE.md"), []byte("x"), 0o600)
	sb := &dockerSandbox{name: "box", workspace: workspace}
	hide, err := sb.currentAgentMasks()
	if err != nil || !cvRContains(hide, "/workspace/CLAUDE.md") {
		t.Fatalf("hide=%v err=%v", hide, err)
	}
	masks, err := sb.scriptMasks(context.Background())
	if err != nil || !cvRContains(masks, "/workspace/CLAUDE.md") {
		t.Fatalf("masks=%v err=%v", masks, err)
	}
	if !maskedAgentCommand(CommandSpec{Provider: "codex"}) || maskedAgentCommand(CommandSpec{Provider: "script"}) || !trustedScriptCommand(CommandSpec{Provider: "script"}) {
		t.Fatal("command classification")
	}
	args := dockerScriptCreateArgs("helper", "base", workspace, "image", "owner")
	if !cvRContains(args, "--cap-add=SYS_ADMIN") {
		t.Fatalf("script args=%v", args)
	}
	m := NewManager(workspace)
	count, err := m.ReclaimCachedDockerImages(context.Background(), nil)
	if err != nil || count != 1 {
		t.Fatalf("manager image reclaim=%d err=%v", count, err)
	}
}

func TestDockerScriptContainerAndExecution(t *testing.T) {
	workspace := t.TempDir()
	cvRInstallFakeDocker(t, workspace)
	gate := filepath.Join(t.TempDir(), "gate-{os}-{arch}")
	gateLinux := strings.ReplaceAll(strings.ReplaceAll(gate, "{os}", "linux"), "{arch}", "amd64")
	if err := os.WriteFile(gateLinux, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_GATE_MCP_BINARY", gate)
	_ = os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("hidden"), 0o600)
	sb := &dockerSandbox{name: "box", workspace: workspace, started: true, profile: domain.SandboxProfile{Environment: map[string]string{"A": "1"}}}
	if err := sb.ensureScriptContainer(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := sb.execScript(context.Background(), CommandSpec{Provider: "script", Argv: []string{"sh", "-c", "echo ok"}, environment: map[string]string{"B": "2"}}, nil)
	if err != nil || !strings.Contains(out, "/workspace") {
		t.Fatalf("exec script=%q err=%v", out, err)
	}
	if err := sb.refreshAgentMasks(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDockerExecRecoversFromUnsafeCWD(t *testing.T) {
	workspace := t.TempDir()
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "failed-once")
	script := `#!/bin/sh
workspace="${CVR_DOCKER_WORKSPACE}"
marker="${CVR_DOCKER_MARKER}"
if [ "${1-}" = inspect ] && [ "${2-}" = -f ]; then echo "$workspace"; exit 0; fi
if [ "${1-}" = inspect ]; then exit 0; fi
if [ "${1-}" = exec ]; then
  case "$*" in *"cd /workspace && pwd"*) echo /workspace; exit 0;; esac
  if [ ! -e "$marker" ]; then
    : > "$marker"
    echo 'OCI runtime exec failed: cwd is outside of container mount namespace'
    exit 126
  fi
  echo recovered
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CVR_DOCKER_WORKSPACE", workspace)
	t.Setenv("CVR_DOCKER_MARKER", marker)
	sb := &dockerSandbox{id: "recover", name: "box", workspace: workspace, started: true}
	out, err := sb.Exec(context.Background(), CommandSpec{Argv: []string{"sh", "-c", "echo x"}}, nil)
	if err != nil || !strings.Contains(out, "recovered") || !sb.started {
		t.Fatalf("out=%q started=%v err=%v", out, sb.started, err)
	}
}

func TestDockerExecInvalidatesMissingContainer(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho 'Error: No such container: box'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	sb := &dockerSandbox{name: "box", workspace: t.TempDir(), started: true}
	if _, err := sb.Exec(context.Background(), CommandSpec{Argv: []string{"true"}}, nil); err == nil || sb.started {
		t.Fatalf("started=%v err=%v", sb.started, err)
	}
}

func cvRInstallFakeDocker(t *testing.T, workspace string) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
set -eu
workspace="${CVR_DOCKER_WORKSPACE}"
if [ "${1-}" = create ] && [ -n "${CVR_DOCKER_CREATE_LOG-}" ]; then
  printf '%s\n' "$@" > "$CVR_DOCKER_CREATE_LOG"
fi
if [ "${1-}" = inspect ] && [ "${2-}" = -f ]; then
  case "${3-}" in
    "{{range"*) printf '%s\n' "$workspace" ;;
    *) printf '/agentflow-cvr\t%s\n' "$workspace" ;;
  esac
  exit 0
fi
if [ "${1-}" = inspect ] && [ "${2-}" = --format ]; then
  case "${3-}" in
    "{{json .}}") echo '{"Id":"base-id","Image":"base-image","State":{"Running":true,"StartedAt":"now"}}'; exit 0 ;;
    "{{json .Mounts}}") echo '[]'; exit 0 ;;
    "{{.State.Running}}|{{.Id}}"*) echo 'true|container-id|now'; exit 0 ;;
    *) echo 'Error: No such container' >&2; exit 1 ;;
  esac
fi
case " $* " in
  *" --workspace-script-view-version "*) echo agentflow-script-view-v2; exit 0 ;;
esac
case "$*" in
  "image inspect --format {{.Architecture}} "*) echo amd64; exit 0 ;;
  "image inspect --format {{with index .Config \"User\"}}{{.}}{{end}} "*) printf '%s' "${CVR_DOCKER_IMAGE_USER-}"; exit 0 ;;
  "image inspect "*) exit 1 ;;
  "images --format {{.Repository}} --filter reference=agentflow-img-*"*) printf 'agentflow-img-a:latest\nagentflow-img-b\nagentflow-img-a\n'; exit 0 ;;
  "ps -a --format {{.Image}}"*) echo agentflow-img-b; exit 0 ;;
  "ps -aq --filter "*) echo id1; exit 0 ;;
  "inspect "*) exit 1 ;;
  "exec "*) echo /workspace; exit 0 ;;
  *) exit 0 ;;
esac
`
	path := filepath.Join(bin, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CVR_DOCKER_WORKSPACE", workspace)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func cvRErrorsJoin(base error, text string) error {
	return &cvRTextError{base: base, text: text}
}

type cvRTextError struct {
	base error
	text string
}

func (e *cvRTextError) Error() string { return e.text + ": " + e.base.Error() }
func (e *cvRTextError) Unwrap() error { return e.base }
