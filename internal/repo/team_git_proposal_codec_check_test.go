package repo

import (
	"encoding/json"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestTeamProposalPublicationCodecDoesNotGrantLegacyEmptyRun(t *testing.T) {
	realRun, emptyRun := "actual-run", ""
	for _, test := range []struct {
		name, kind, run string
		sqlRun          *string
		valid           bool
	}{
		{"typed-proposal-null-run", "teamai_proposal", "", nil, true},
		{"typed-proposal-fabricated-run", "teamai_proposal", realRun, &realRun, false},
		{"legacy-real-run", "", realRun, &realRun, true},
		{"legacy-null-run", "", "", nil, false},
		{"legacy-nonnull-empty-run", "", "", &emptyRun, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			publication := domain.ContributionPublication{ID: strings.Repeat("1", 64), SourceKind: test.kind, RunID: test.run, ProjectID: "project", SourceSHA256: strings.Repeat("2", 64)}
			raw, err := json.Marshal(publication)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodePublication(contributionPublicationRow{ID: publication.ID, RunID: test.sqlRun, Payload: string(raw)})
			if (err == nil) != test.valid {
				t.Fatalf("publication codec accepted=%v expected=%v", err == nil, test.valid)
			}
		})
	}
}
