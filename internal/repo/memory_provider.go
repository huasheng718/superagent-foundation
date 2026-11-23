package repo

import (
	"encoding/json"
	"errors"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrMemoryIndexCapacity = errors.New("governed memory provider index capacity exceeded")

type memoryProviderMirrorRow struct {
	ID          string    `gorm:"column:id;primaryKey;size:64"`
	ProjectID   string    `gorm:"column:project_id;size:64;not null;index:idx_memory_provider_scope,priority:1"`
	ScopeSHA256 string    `gorm:"column:scope_sha256;size:64;not null;index:idx_memory_provider_scope,priority:2"`
	State       string    `gorm:"column:state;size:32;not null"`
	Payload     []byte    `gorm:"column:payload;type:json;not null"`
	CreatedAt   time.Time `gorm:"column:created_at;not null"`
	UpdatedAt   time.Time `gorm:"column:updated_at;not null"`
}

func (memoryProviderMirrorRow) TableName() string { return "memory_provider_mirrors" }
func memoryScopeDigest(scope domain.MemoryProviderScope, binding domain.MemoryProviderBinding) string {
	sha, _ := domain.IntelligenceDigest(struct {
		Scope   domain.MemoryProviderScope
		Binding domain.MemoryProviderBinding
	}{scope, binding})
	return sha
}
func currentMemoryPin(m domain.GovernedMemory, revision int, sha string) bool {
	return m.State == domain.MemoryApproved && m.Revision == revision && m.SHA256 == sha && (m.ExpiresAt == nil || m.ExpiresAt.After(time.Now().UTC()))
}
func (r *MemoryRepo) MemoryProviderPin(project, id string, revision int, sha string) (domain.GovernedMemory, error) {
	m, err := r.GovernedMemory(project, id)
	if err != nil {
		return m, err
	}
	if !currentMemoryPin(m, revision, sha) {
		return m, ErrIntelligenceUnavailable
	}
	return m, nil
}

// EligibleProviderMemories refuses overflow instead of calling a partial list complete.
func (r *MemoryRepo) EligibleProviderMemories(project string) ([]domain.GovernedMemory, error) {
	var rows []governedMemoryRow
	if err := r.DB.Where("project_id = ? AND state = ?", project, domain.MemoryApproved).Order("id").Limit(501).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) > 500 {
		return nil, ErrMemoryIndexCapacity
	}
	out := []domain.GovernedMemory{}
	size := 0
	for _, row := range rows {
		m, err := decodeGovernedMemory(row)
		if err != nil {
			return nil, err
		}
		if !currentMemoryPin(m, m.Revision, m.SHA256) {
			continue
		}
		size += len(m.Title) + len(m.Content)
		if size > 8<<20 {
			return nil, ErrMemoryIndexCapacity
		}
		out = append(out, m)
	}
	return out, nil
}
func decodeMemoryMirror(row memoryProviderMirrorRow) (domain.MemoryProviderMirror, error) {
	var m domain.MemoryProviderMirror
	if json.Unmarshal(row.Payload, &m) != nil || m.ID != row.ID || m.Scope.ProjectID != row.ProjectID || m.State != row.State || memoryScopeDigest(m.Scope, m.Binding) != row.ScopeSHA256 || domain.MemoryMirrorID(m.Scope, m.Binding, m.MemoryID, m.Revision, m.SHA256) != m.ID || m.SessionID != "lfm-"+m.ID {
		return m, ErrIntelligenceConflict
	}
	if m.State != "pending" && m.State != "ready" && m.State != "delete_pending" && m.State != "deleted" {
		return m, ErrIntelligenceConflict
	}
	if len(m.Chunks) < 1 || len(m.Chunks) > 4 {
		return m, ErrIntelligenceConflict
	}
	seen := map[string]bool{}
	for _, chunk := range m.Chunks {
		if chunk.SHA256 == "" || chunk.Size < 1 || chunk.Size > 7000 {
			return m, ErrIntelligenceConflict
		}
		if m.State == "ready" && (chunk.RecordID == "" || chunk.Version != "v1") {
			return m, ErrIntelligenceConflict
		}
		if chunk.RecordID != "" {
			if seen[chunk.RecordID] {
				return m, ErrIntelligenceConflict
			}
			seen[chunk.RecordID] = true
		}
	}
	return m, nil
}
func (r *MemoryRepo) MemoryProviderMirrors(scope domain.MemoryProviderScope, binding domain.MemoryProviderBinding) ([]domain.MemoryProviderMirror, error) {
	var rows []memoryProviderMirrorRow
	if err := r.DB.Where("project_id = ? AND scope_sha256 = ?", scope.ProjectID, memoryScopeDigest(scope, binding)).Order("id").Limit(2001).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) > 2000 {
		return nil, ErrMemoryIndexCapacity
	}
	out := []domain.MemoryProviderMirror{}
	for _, row := range rows {
		m, err := decodeMemoryMirror(row)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// ReserveMemoryMirror is the only add permission. A durable existing intent is
// never permission to append again, even if discovery currently returns empty.
func (r *MemoryRepo) ReserveMemoryMirror(m domain.MemoryProviderMirror) (domain.MemoryProviderMirror, bool, error) {
	var out domain.MemoryProviderMirror
	created := false
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var native governedMemoryRow
		if err := r.locking(tx).Where("id = ? AND project_id = ?", m.MemoryID, m.Scope.ProjectID).Take(&native).Error; err != nil {
			return ErrIntelligenceUnavailable
		}
		memory, err := decodeGovernedMemory(native)
		if err != nil {
			return err
		}
		if !currentMemoryPin(memory, m.Revision, m.SHA256) {
			return ErrIntelligenceUnavailable
		}
		if domain.MemoryMirrorID(m.Scope, m.Binding, m.MemoryID, m.Revision, m.SHA256) != m.ID || m.SessionID != "lfm-"+m.ID || m.State != "pending" {
			return ErrIntelligenceConflict
		}
		m.IntentAt = time.Now().UTC()
		raw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		row := memoryProviderMirrorRow{ID: m.ID, ProjectID: m.Scope.ProjectID, ScopeSHA256: memoryScopeDigest(m.Scope, m.Binding), State: m.State, Payload: raw, CreatedAt: m.IntentAt, UpdatedAt: m.IntentAt}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if result.Error != nil {
			return result.Error
		}
		created = result.RowsAffected == 1
		if err := tx.Where("id = ?", m.ID).Take(&row).Error; err != nil {
			return err
		}
		out, err = decodeMemoryMirror(row)
		return err
	})
	return out, created, err
}

// CompleteMemoryMirror performs native approval revalidation under its row lock.
func (r *MemoryRepo) CompleteMemoryMirror(id string, pins []domain.MemoryProviderChunk) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var row memoryProviderMirrorRow
		if err := tx.Where("id = ?", id).Take(&row).Error; err != nil {
			return err
		}
		m, err := decodeMemoryMirror(row)
		if err != nil {
			return err
		}
		var native governedMemoryRow
		if err := r.locking(tx).Where("id = ? AND project_id = ?", m.MemoryID, m.Scope.ProjectID).Take(&native).Error; err != nil {
			return ErrIntelligenceUnavailable
		}
		memory, err := decodeGovernedMemory(native)
		if err != nil {
			return err
		}
		if !currentMemoryPin(memory, m.Revision, m.SHA256) {
			return ErrIntelligenceUnavailable
		}
		if m.State != "pending" {
			if m.State == "ready" {
				return nil
			}
			return ErrIntelligenceConflict
		}
		if len(pins) != len(m.Chunks) {
			return ErrIntelligenceConflict
		}
		type chunkKey struct {
			SHA256 string
			Size   int
		}
		expected := map[chunkKey]int{}
		for _, p := range m.Chunks {
			expected[chunkKey{p.SHA256, p.Size}]++
		}
		seen := map[string]bool{}
		for _, p := range pins {
			if p.RecordID == "" || p.Version != "v1" || seen[p.RecordID] || expected[chunkKey{p.SHA256, p.Size}] == 0 {
				return ErrIntelligenceConflict
			}
			seen[p.RecordID] = true
			expected[chunkKey{p.SHA256, p.Size}]--
		}
		m.State = "ready"
		m.Chunks = pins
		m.LastError = ""
		raw, _ := json.Marshal(m)
		result := tx.Model(&memoryProviderMirrorRow{}).Where("id = ? AND state = ?", id, "pending").Updates(map[string]any{"state": "ready", "payload": raw, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrIntelligenceConflict
		}
		return nil
	})
}
func (r *MemoryRepo) SetMemoryMirrorState(id, from, to, errorClass string) error {
	if !(from == "pending" && to == "pending" || from == "pending" && to == "delete_pending" || from == "ready" && to == "delete_pending" || from == "delete_pending" && to == "deleted") {
		return ErrIntelligenceConflict
	}
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var row memoryProviderMirrorRow
		if err := r.locking(tx).Where("id = ?", id).Take(&row).Error; err != nil {
			return err
		}
		m, err := decodeMemoryMirror(row)
		if err != nil {
			return err
		}
		if m.State != from {
			return ErrIntelligenceConflict
		}
		m.State = to
		m.LastError = errorClass
		raw, _ := json.Marshal(m)
		return tx.Model(&memoryProviderMirrorRow{}).Where("id = ? AND state = ?", id, from).Updates(map[string]any{"state": to, "payload": raw, "updated_at": time.Now().UTC()}).Error
	})
}
