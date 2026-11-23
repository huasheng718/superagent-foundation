package repo

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"agentflow/internal/domain"
	mysql "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// These connections use MySQL's production REPEATABLE READ semantics. An
// earlier consistent read is deliberately opened before a second pool commits
// new policy/files and before capture locks that source's current row.
func TestMySQLReleaseDistributionCurrentReadAcrossStores(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_DISTRIBUTION_MYSQL_DSN")
	if dsn == "" {
		t.Skip("disposable distribution MySQL DSN required")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || cfg == nil || os.Getenv("AGENTFLOW_DISTRIBUTION_MYSQL_DISPOSABLE") != "1" || !strings.HasPrefix(cfg.DBName, "distribution_acceptance_") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("explicit loopback disposable distribution schema required; DSN omitted")
	}
	a, b := budgetMySQLStore(t, dsn), budgetMySQLStore(t, dsn)
	defer a.Close()
	defer b.Close()
	for _, id := range []string{"release-read-source", "release-read-target"} {
		if err := a.PutProject(domain.Project{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	installHook := func(table string, change func() error) *bool {
		t.Helper()
		applied := new(bool)
		key := "release_distribution_current_read"
		if err := a.DB.Callback().Query().Before("gorm:query").Register(key, func(tx *gorm.DB) {
			if tx.Statement.Table != table || *applied {
				return
			}
			if _, ok := tx.Statement.Clauses["FOR"]; !ok {
				return
			}
			*applied = true
			var count int64
			// Reuse the actual transaction connection without inheriting its FOR UPDATE
			// source query; this establishes a historical consistent-read snapshot.
			if err := tx.Session(&gorm.Session{NewDB: true}).Raw("SELECT COUNT(*) FROM rules").Scan(&count).Error; err != nil {
				tx.AddError(err)
				return
			}
			if err := change(); err != nil {
				tx.AddError(err)
			}
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.DB.Callback().Query().Remove(key) })
		return applied
	}
	t.Run("latest_copy_deny_is_authority", func(t *testing.T) {
		if err := a.PutRule(domain.RuleDefinition{ID: "release-latest-rule", Name: "Rule", ProjectID: "release-read-source", Shared: true, Content: "inspectable"}); err != nil {
			t.Fatal(err)
		}
		if err := a.PutAgent(domain.AgentDefinition{ID: "release-rule-agent", Name: "Agent", ProjectID: "release-read-source", Shared: true, Rules: []string{"release-latest-rule"}}); err != nil {
			t.Fatal(err)
		}
		observed, _ := b.Rule("release-latest-rule")
		applied := installHook("rules", func() error {
			_, err := b.SetDistribution("rule", observed.ID, observed.SourceRevision, domain.DistributionPolicy{Copy: "deny", Edit: "allow", Export: "allow"}, "restriction committed before source current read", DistributionActor{Local: true})
			return err
		})
		_, err := a.CreateTeamRelease("release-read-target", domain.TeamReleaseRequest{Name: "Must refuse latest restriction", AgentIDs: []string{"release-rule-agent"}}, "human")
		if !*applied || !errors.Is(err, domain.ErrDistributionDenied) {
			t.Fatalf("capture admitted historical allow: applied=%t error=%v", *applied, err)
		}
		list, err := b.TeamReleases("release-read-target")
		if err != nil || len(list) != 0 {
			t.Fatal("denied source partially published", err)
		}
		a.DB.Callback().Query().Remove("release_distribution_current_read")
	})
	t.Run("skill_definition_policy_and_binary_are_current", func(t *testing.T) {
		if err := a.PutSkill(domain.SkillDefinition{ID: "release-latest-skill", Name: "Skill", ProjectID: "release-read-source", Shared: true, Content: "Reviewed instructions"}); err != nil {
			t.Fatal(err)
		}
		skill, _ := a.Skill("release-latest-skill")
		if err := a.PutSkillFiles(skill.ID, []SkillFileInput{{Path: "data.bin", Content: []byte("old-data"), Mode: 0644}}, skill.SourceRevision); err != nil {
			t.Fatal(err)
		}
		skill, _ = b.Skill(skill.ID)
		if _, err := b.SetDistribution("skill", skill.ID, skill.SourceRevision, domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "deny"}, "retain export floor with latest files", DistributionActor{Local: true}); err != nil {
			t.Fatal(err)
		}
		skill, _ = b.Skill(skill.ID)
		latest := []byte("new-current-binary")
		applied := installHook("skills", func() error {
			return b.PutSkillFiles(skill.ID, []SkillFileInput{{Path: "data.bin", Content: latest, Mode: 0755}}, skill.SourceRevision)
		})
		release, err := a.CreateTeamRelease("release-read-target", domain.TeamReleaseRequest{Name: "Current skill", SkillIDs: []string{skill.ID}}, "human")
		if err != nil || !*applied {
			t.Fatalf("current capture failed: applied=%t error=%v", *applied, err)
		}
		frozen := release.Payload.Assets.Skills[0]
		current, _ := b.Skill(skill.ID)
		if !bytes.Equal(release.Payload.Assets.SkillFiles[skill.ID]["data.bin"], latest) || frozen.SourceRevision != current.SourceRevision || frozen.FileRefs[0].Mode != 0755 || frozen.DistributionState.Effective().Export != "deny" {
			t.Fatal("skill bytes/token/mode/floor are not from the locked current source")
		}
		a.DB.Callback().Query().Remove("release_distribution_current_read")
	})
	t.Run("mcp_binary_uses_current_parent_manifest", func(t *testing.T) {
		if err := a.PutMCPServer(domain.MCPServerDefinition{ID: "release-latest-mcp", Name: "MCP", Scope: "project", ProjectID: "release-read-target", Transport: "stdio", Command: "sh"}); err != nil {
			t.Fatal(err)
		}
		mcp, _ := a.MCPServer("release-latest-mcp")
		if err := a.PutMCPFiles(mcp.ID, []SkillFileInput{{Path: "main.sh", Content: []byte("echo old"), Mode: 0644}}, mcp.SourceRevision); err != nil {
			t.Fatal(err)
		}
		mcp, _ = b.MCPServer(mcp.ID)
		latest := []byte("echo current reviewed binary")
		applied := installHook("mcp_servers", func() error {
			return b.PutMCPFiles(mcp.ID, []SkillFileInput{{Path: "main.sh", Content: latest, Mode: 0755}}, mcp.SourceRevision)
		})
		release, err := a.CreateTeamRelease("release-read-target", domain.TeamReleaseRequest{Name: "Current MCP", MCPServerIDs: []string{mcp.ID}, ExpectedBaseVersion: 1}, "human")
		if err != nil || !*applied {
			t.Fatalf("current MCP capture failed: applied=%t error=%v", *applied, err)
		}
		frozen := release.Payload.Assets.MCPServers[0]
		current, _ := b.MCPServer(mcp.ID)
		if !bytes.Equal(release.Payload.Assets.MCPFiles[mcp.ID]["main.sh"], latest) || frozen.SourceRevision != current.SourceRevision || frozen.FileRefs[0].Mode != 0755 {
			t.Fatal("MCP bytes/token/mode are not from the locked current source")
		}
		a.DB.Callback().Query().Remove("release_distribution_current_read")
	})
}
