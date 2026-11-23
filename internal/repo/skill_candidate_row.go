package repo

import "time"

type skillCandidateRow struct {
	ID        string    `gorm:"column:id;primaryKey;size:64"`
	ProjectID string    `gorm:"column:project_id;index:idx_skill_candidate_scope,priority:1;size:64;not null"`
	State     string    `gorm:"column:state;index:idx_skill_candidate_scope,priority:2;size:32;not null"`
	Revision  int       `gorm:"column:revision;not null"`
	Payload   []byte    `gorm:"column:payload;type:json;not null"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
}

func (skillCandidateRow) TableName() string { return "skill_candidates" }
