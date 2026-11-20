package repo

import (
	"agentflow/internal/domain"
	"encoding/json"
	"time"
)

type memoryRecallReceiptRow struct {
	ID        string    `gorm:"column:id;primaryKey;size:64"`
	ProjectID string    `gorm:"column:project_id;size:64;not null;index:idx_memory_receipt_scope"`
	UserID    int64     `gorm:"column:user_id;not null"`
	Payload   []byte    `gorm:"column:payload;type:json;not null"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
}

func (memoryRecallReceiptRow) TableName() string { return "memory_recall_receipts" }
func (r *MemoryRepo) SaveMemoryRecallReceipt(value domain.MemoryRecallReceipt) (domain.MemoryRecallReceipt, error) {
	var err error
	if value.Scope.ProjectID == "" || value.Scope.UserID <= 0 || value.Scope.AgentID == "" || len(value.Hits) > domain.MemoryMaxRecallItems {
		return value, ErrIntelligenceUnavailable
	}
	value.ID, err = intelligenceID("recall-")
	if err != nil {
		return value, err
	}
	value.CreatedAt = time.Now().UTC()
	value.ExpiresAt = value.CreatedAt.Add(10 * time.Minute)
	value.SHA256, err = value.Digest()
	if err != nil {
		return value, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return value, err
	}
	err = r.DB.Create(&memoryRecallReceiptRow{ID: value.ID, ProjectID: value.Scope.ProjectID, UserID: value.Scope.UserID, Payload: raw, CreatedAt: value.CreatedAt}).Error
	return value, err
}

// Selection checks TTL. Frozen replay deliberately does not select or refresh.
func (r *MemoryRepo) MemoryRecallReceipt(project, id string, user int64, selection bool) (domain.MemoryRecallReceipt, error) {
	var row memoryRecallReceiptRow
	var out domain.MemoryRecallReceipt
	if user <= 0 || r.DB.Where("id = ? AND project_id = ? AND user_id = ?", id, project, user).Take(&row).Error != nil {
		return out, ErrIntelligenceUnavailable
	}
	if json.Unmarshal(row.Payload, &out) != nil || out.ID != row.ID || out.Scope.ProjectID != row.ProjectID || out.Scope.UserID != row.UserID {
		return out, ErrIntelligenceConflict
	}
	digest, err := out.Digest()
	if err != nil || digest != out.SHA256 {
		return out, ErrIntelligenceConflict
	}
	if selection && !out.ExpiresAt.After(time.Now().UTC()) {
		return out, ErrIntelligenceUnavailable
	}
	return out, nil
}
