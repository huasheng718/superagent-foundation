package httpapi

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

// The distributed skill must survive the same ZIP import used by the UI and
// run from a different working directory, without access to this repository.
func TestUsageSkillZipPortable(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "false")
	t.Setenv("AGENTFLOW_SCHEDULER_ENABLED", "false")
	s := newSkillFileServer(t)
	h := skillFileHandler(t, s)
	if err := s.PutSkill(domain.SkillDefinition{ID: "report-workflow-usage", Name: "外部工作流用量上报", ProjectID: "platform"}); err != nil {
		t.Fatal(err)
	}
	skill, _ := s.Skill("report-workflow-usage")
	root := filepath.Join("..", "..", "skills", "report-workflow-usage")
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target, err := writer.Create("report-workflow-usage/" + filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		_, err = target.Write(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	rec := requestMultipart(t, h, http.MethodPost, "/api/skills/report-workflow-usage/files", func(form *multipart.Writer) {
		part, err := form.CreateFormFile("files", "report-workflow-usage.zip")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(archive.Bytes()); err != nil {
			t.Fatal(err)
		}
	}, map[string]string{"If-Match": `"` + skill.SourceRevision + `"`})
	if rec.Code != 200 {
		t.Fatalf("ZIP import: %d %s", rec.Code, rec.Body.String())
	}
	isolated := t.TempDir()
	contents := s.SkillFileContents("report-workflow-usage")
	for _, required := range []string{"SKILL.md", "scripts/report_usage.py", "references/contract.md", "references/workflow-integration.md", "agents/openai.yaml"} {
		if len(contents[required]) == 0 {
			t.Fatalf("missing %s after import", required)
		}
	}
	for rel, body := range contents {
		path := filepath.Join(isolated, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	workspace := t.TempDir()
	file := filepath.Join(workspace, "usage.json")
	now := time.Now().UTC().Add(-time.Minute)
	report := map[string]any{"id": "event-1", "type": "workflow_execution", "externalExecutionId": "external-1", "workflow": map[string]any{"id": "flow", "name": "Flow"}, "startedAt": now.Add(-time.Minute).Format(time.RFC3339), "finishedAt": now.Format(time.RFC3339), "status": "failed", "usage": []map[string]any{{"provider": "p", "model": "m", "inputTokens": 10, "outputTokens": 5, "costStatus": "unknown", "costUsd": nil}}}
	run := func(t *testing.T, wantOK bool) {
		t.Helper()
		body, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, body, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(python, filepath.Join(isolated, "scripts", "report_usage.py"), "--check")
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(), "AGENTFLOW_USAGE_FILE="+file, "AGENTFLOW_INSIGHT_TOKEN=", "AGENTFLOW_URL=")
		output, err := cmd.CombinedOutput()
		if (err == nil) != wantOK {
			t.Fatalf("check: %v %s", err, output)
		}
		if wantOK && !strings.Contains(string(output), `"status": "valid"`) {
			t.Fatalf("no validation receipt: %s", output)
		}
		if _, err := os.Stat(file); err != nil {
			t.Fatal("report file removed")
		}
	}
	run(t, true)
	// Go's RFC3339Nano removes trailing zeroes. Exercise every resulting
	// precision against the distributed CLI, including Python 3.9/3.10.
	for _, zone := range []string{"Z", "+08:00", "-03:30"} {
		for digits := 0; digits <= 9; digits++ {
			fraction := ""
			if digits > 0 {
				fraction = "." + "123456789"[:digits]
			}
			t.Run("fraction"+fraction+zone, func(t *testing.T) {
				report["startedAt"] = "2020-01-02T03:04:04" + fraction + zone
				report["finishedAt"] = "2020-01-02T03:04:05" + fraction + zone
				run(t, true)
			})
		}
	}
	report["startedAt"] = "2020-01-02T03:04:04Z"
	for _, invalid := range []string{"2020-01-02T03:04:05.123", "2020-02-30T03:04:05.123Z", "2020-01-02T03:04:05.Z", "2020-01-02T03:04:03Z"} {
		t.Run("invalid-time-"+invalid, func(t *testing.T) {
			report["finishedAt"] = invalid
			run(t, false)
		})
	}
	report["finishedAt"] = "2020-01-02T03:04:05Z"
	item := report["usage"].([]map[string]any)[0]
	delete(item, "inputTokens")
	run(t, false) // Missing real counts cannot become fabricated zeroes.
	item["inputTokens"] = 0
	item["costStatus"] = "reported"
	item["costUsd"] = 0
	run(t, true)
	item["costUsd"] = 1e300
	run(t, false)
	item["costUsd"] = 0
	item["costStatus"] = "unknown"
	run(t, false) // Unknown cost cannot masquerade as zero.
	item["costUsd"] = nil
	report["projectId"] = "another-project"
	run(t, false)
	delete(report, "projectId")
	report["status"] = "running"
	run(t, false)
}
