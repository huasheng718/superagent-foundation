package repo

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	mysql "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestMySQLDistributionPolicyCrossStore(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_DISTRIBUTION_MYSQL_DSN")
	if dsn == "" {
		t.Skip("disposable distribution MySQL DSN not supplied")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || cfg == nil || os.Getenv("AGENTFLOW_DISTRIBUTION_MYSQL_DISPOSABLE") != "1" || !strings.HasPrefix(cfg.DBName, "distribution_acceptance_") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("explicit loopback disposable distribution schema required; DSN omitted")
	}
	a, b := budgetMySQLStore(t, dsn), budgetMySQLStore(t, dsn)
	defer a.Close()
	defer b.Close()
	if err := a.PutProject(domain.Project{ID: "distribution-race", Name: "Race", OwnerID: 101}); err != nil {
		t.Fatal(err)
	}
	skill, err := a.SaveSkillMetadata(domain.SkillDefinition{ID: "distribution-race-skill", Name: "Skill", ProjectID: "distribution-race"})
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-gate
		_, err := a.SetDistribution("skill", skill.ID, skill.SourceRevision, domain.DistributionPolicy{Copy: "deny", Edit: "deny", Export: "deny"}, "concurrent owner policy", DistributionActor{UserID: 101})
		results <- err
	}()
	go func() {
		<-gate
		_, err := b.SaveSkillFiles(skill.ID, []SkillFileInput{{Path: "script.sh", Content: []byte("echo reviewed"), Mode: 0755}}, skill.SourceRevision)
		results <- err
	}()
	close(gate)
	wins, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, ErrSourceConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("policy/file same-base: wins=%d conflicts=%d", wins, conflicts)
	}
	// Hold an actual project-row transfer in another connection while the old
	// owner's policy decision waits. It must check the owner after taking lock.
	rule, err := a.SaveRule(domain.RuleDefinition{ID: "distribution-owner-rule", Name: "Rule", ProjectID: "distribution-race"})
	if err != nil {
		t.Fatal(err)
	}
	tx := b.DB.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	var row projectRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", "distribution-race").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Model(&projectRow{}).Where("id = ?", row.ID).Update("owner_user_id", 102).Error; err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := a.SetDistribution("rule", rule.ID, rule.SourceRevision, domain.DistributionPolicy{Copy: "allow", Edit: "deny", Export: "deny"}, "stale owner request", DistributionActor{UserID: 101})
		done <- err
	}()
	<-started
	// The completion cannot occur while the owning project row is locked.
	select {
	case err := <-done:
		t.Fatalf("policy did not wait on owner row: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrDistributionOwner) {
		t.Fatalf("old owner admitted after transfer: %v", err)
	}
	current, _ := b.Rule(rule.ID)
	if current.Distribution != nil || current.SourceRevision != rule.SourceRevision {
		t.Fatal("denied transfer race wrote policy")
	}
	t.Log("two independent MySQL pools: source policy/file race one commit one conflict; old-owner decision denied after locked transfer; no source mutation")
}

// A caller can legitimately hold an older consistent-read snapshot while it
// obtains a fresh CAS token from an owner decision on another connection.
// editSource's fresh lock must also govern the final policy-preserving write.
func TestMySQLCatalogDistributionGuardCurrentRead(t *testing.T) {
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
	for _, edit := range []string{"allow", "deny"} {
		t.Run("latest_edit_"+edit, func(t *testing.T) {
			id := "guard-current-" + edit
			if err := a.PutProject(domain.Project{ID: id, Name: id}); err != nil {
				t.Fatal(err)
			}
			initial, err := a.SaveRule(domain.RuleDefinition{ID: id, Name: "Rule", ProjectID: id, Content: "original"})
			if err != nil {
				t.Fatal(err)
			}
			var writeErr error
			err = a.DB.Transaction(func(tx *gorm.DB) error {
				store := newStore(tx)
				old, ok := store.Rule(id)
				if !ok {
					return ErrIntelligenceUnavailable
				}
				decision, err := b.SetDistribution("rule", id, initial.SourceRevision, domain.DistributionPolicy{Copy: "allow", Edit: edit, Export: "deny"}, "owner decision committed after caller snapshot", DistributionActor{Local: true})
				if err != nil {
					return err
				}
				old.SourceRevision = decision.SourceRevision
				old.Content = "newly edited"
				_, writeErr = store.SaveRule(old)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			current, _ := b.Rule(id)
			if edit == "deny" {
				if !errors.Is(writeErr, domain.ErrDistributionDenied) || current.Content != "original" || current.DistributionState.Effective().Edit != "deny" {
					t.Fatalf("old snapshot bypassed current editdeny: err=%v content=%s policy=%+v", writeErr, current.Content, current.DistributionState.Effective())
				}
			} else if writeErr != nil || current.Content != "newly edited" || current.DistributionState.Effective().Export != "deny" {
				t.Fatalf("old snapshot erased current export policy: err=%v content=%s policy=%+v", writeErr, current.Content, current.DistributionState.Effective())
			}
		})
	}
}
