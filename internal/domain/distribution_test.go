package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDistributionLegacyAndInheritedFloor(t *testing.T) {
	legacy := RuleDefinition{ID: "r", Name: "Rule"}
	raw, _ := json.Marshal(legacy)
	if strings.Contains(string(raw), "distribution") {
		t.Fatal("legacy wire changed")
	}
	if err := CheckDistribution(legacy, "copy"); err != nil {
		t.Fatal(err)
	}
	source := legacy
	source.Distribution = &DistributionPolicy{Copy: "allow", Edit: "deny", Export: "deny"}
	copied, err := CopyDistribution(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied.DistributionOrigins) != 1 || copied.DistributionOrigins[0].SHA256 == "" {
		t.Fatal(copied)
	}
	copied.Distribution = nil
	if !errors.Is(copied.Check("edit"), ErrDistributionDenied) || !errors.Is(copied.Check("export"), ErrDistributionDenied) {
		t.Fatal("lost inherited floor")
	}
	target := RuleDefinition{ID: "new", Name: "New", ProjectID: "different", DistributionState: copied}
	again, err := CopyDistribution(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.DistributionOrigins) != 2 || again.Effective().Edit != "deny" {
		t.Fatal(again)
	}
	source.Distribution.Copy = "deny"
	if _, err := CopyDistribution(source); !errors.Is(err, ErrDistributionDenied) {
		t.Fatal(err)
	}
	if err := CheckDistribution(struct{}{}, "export"); !errors.Is(err, ErrDistributionInvalid) {
		t.Fatal("unknown type must fail closed")
	}
}
func TestDistributionRejectsPartialAndOverlongOrigins(t *testing.T) {
	if err := (DistributionState{Distribution: &DistributionPolicy{Copy: "allow"}}).Validate(); err == nil {
		t.Fatal("partial accepted")
	}
	if err := (DistributionState{DistributionOrigins: make([]DistributionOrigin, 65)}).Validate(); err == nil {
		t.Fatal("unbounded origins")
	}
}
