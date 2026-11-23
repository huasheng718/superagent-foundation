package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agentflow/internal/domain"

	"gorm.io/gorm"
)

// SkillFileInput is one binary file to persist for a skill.
type SkillFileInput struct {
	Path    string
	Content []byte
	Mode    uint32
}

// SkillFileRefs returns the binary manifest of a skill, sorted by path.
func (r *CatalogRepo) SkillFileRefs(skillID string) []domain.SkillFileRef {
	var rows []skillFileRow
	if err := r.DB.Where("skill_id = ?", skillID).Order("path").Find(&rows).Error; err != nil {
		return nil
	}
	out := make([]domain.SkillFileRef, 0, len(rows))
	for _, row := range rows {
		ref := domain.SkillFileRef{Path: row.Path, Size: row.Size, SHA256: row.SHA256, Mode: row.UnixMode}
		if row.Updated != nil {
			ref.UpdatedAt = *row.Updated
		}
		out = append(out, ref)
	}
	return out
}

// SkillFileContent loads one binary file of a skill.
func (r *CatalogRepo) SkillFileContent(skillID, path string) ([]byte, bool) {
	var row skillFileRow
	if err := r.DB.Where("skill_id = ? AND path = ?", skillID, path).Take(&row).Error; err != nil {
		return nil, false
	}
	return row.Content, true
}

// SkillFileContents loads every binary file of a skill (path -> bytes).
func (r *CatalogRepo) SkillFileContents(skillID string) map[string][]byte {
	var rows []skillFileRow
	if err := r.DB.Where("skill_id = ?", skillID).Find(&rows).Error; err != nil {
		return nil
	}
	out := make(map[string][]byte, len(rows))
	for _, row := range rows {
		out[row.Path] = row.Content
	}
	return out
}

// SkillFileTotalBytes returns the combined size of the binary files.
func (r *CatalogRepo) SkillFileTotalBytes(skillID string) int64 {
	var total int64
	if err := r.DB.Table("skill_files").Where("skill_id = ?", skillID).
		Select("COALESCE(SUM(size),0)").Scan(&total).Error; err != nil {
		return 0
	}
	return total
}

// PutSkillFiles stores the given binary files as the complete file set of
// the skill (replace semantics): existing rows for the skill are removed
// before the new ones are inserted, all inside one transaction. The skill
// payload manifest is refreshed from the resulting rows.
func (r *CatalogRepo) PutSkillFiles(skillID string, files []SkillFileInput, expected ...string) error {
	_, err := r.SaveSkillFiles(skillID, files, firstSourceRevision(expected))
	return err
}

func (r *CatalogRepo) SaveSkillFiles(skillID string, files []SkillFileInput, expected string) (domain.SkillDefinition, error) {
	var saved domain.SkillDefinition
	if skillID == "" || len(files) == 0 {
		return saved, errors.New("skill id and files are required")
	}
	err := r.editSource("skills", skillID, expected, func(catalog *CatalogRepo, raw []byte, token string, exists bool) error {
		if !exists {
			return ErrSourceConflict
		}
		var skill domain.SkillDefinition
		if err := json.Unmarshal(raw, &skill); err != nil {
			return err
		}
		if skill.Source != nil {
			return ErrSkillSourceReadOnly
		}
		now := time.Now().UTC()
		skill.SourceRevision = token
		skill.UpdatedAt = now
		if err := writeSource(catalog.DB, "skills", map[string]any{"id": skill.ID, "name": skill.Name, "payload": mustJSON(skill), "updated_at": timePtr(now)}, true); err != nil {
			return err
		}
		if err := catalog.DB.Where("skill_id = ?", skillID).Delete(&skillFileRow{}).Error; err != nil {
			return err
		}
		for _, file := range files {
			if _, ok := skill.Files[file.Path]; ok {
				return fmt.Errorf("path conflicts with text resource")
			}
			if err := writeSkillFileRow(catalog.DB, skillID, file, now); err != nil {
				return err
			}
		}
		if err := refreshSkillFileRefs(catalog.DB, skillID, now); err != nil {
			return err
		}
		saved, _ = catalog.Skill(skillID)
		return nil
	})
	return saved, err
}

// ErrSkillExists is returned by PutSkillSnapshot when create is requested
// for an id that already has a skills row.
var ErrSkillExists = errors.New("skill id already exists")

// PutSkillSnapshot writes the skill row and its complete file set in one
// transaction, so a failed install or update leaves no partial skill and no
// half-replaced files. With create=true the id must not exist yet.
func (r *CatalogRepo) PutSkillSnapshot(v domain.SkillDefinition, files []SkillFileInput, create bool) error {
	_, err := r.SaveSkillSnapshot(v, files, create)
	return err
}

func (r *CatalogRepo) SaveSkillSnapshot(v domain.SkillDefinition, files []SkillFileInput, create bool) (domain.SkillDefinition, error) {
	var saved domain.SkillDefinition
	if v.ID == "" || v.Name == "" || len(files) == 0 {
		return saved, errors.New("skill id, name and files are required")
	}
	if create {
		v.SourceRevision = ""
	}
	err := r.editSource("skills", v.ID, v.SourceRevision, func(catalog *CatalogRepo, raw []byte, token string, exists bool) error {
		if create && exists {
			return ErrSkillExists
		}
		if !create && !exists {
			return ErrSourceConflict
		}
		now := time.Now().UTC()
		v.SourceRevision = token
		v.UpdatedAt = now
		values := map[string]any{"id": v.ID, "name": v.Name, "payload": mustJSON(v), "updated_at": timePtr(now)}
		if err := writeSource(catalog.DB, "skills", values, exists); err != nil {
			if create && errors.Is(err, ErrSourceConflict) {
				return ErrSkillExists
			}
			return err
		}
		if err := catalog.DB.Where("skill_id = ?", v.ID).Delete(&skillFileRow{}).Error; err != nil {
			return err
		}
		for _, file := range files {
			if err := writeSkillFileRow(catalog.DB, v.ID, file, now); err != nil {
				return err
			}
		}
		if err := refreshSkillFileRefs(catalog.DB, v.ID, now); err != nil {
			return err
		}
		saved, _ = catalog.Skill(v.ID)
		return nil
	})
	if create && errors.Is(err, ErrSourcePrecondition) {
		err = ErrSkillExists
	}
	return saved, err
}

func writeSkillFileRow(tx *gorm.DB, skillID string, file SkillFileInput, now time.Time) error {
	if err := domain.ValidateRelativeFilePath(file.Path); err != nil {
		return err
	}
	if int64(len(file.Content)) > domain.SkillFileMaxBytes {
		return fmt.Errorf("skill 文件 %s 超过 1MB 限制", file.Path)
	}
	sum := sha256.Sum256(file.Content)
	return upsert(tx, "skill_files", map[string]any{
		"skill_id":   skillID,
		"path":       file.Path,
		"content":    file.Content,
		"size":       int64(len(file.Content)),
		"sha256":     hex.EncodeToString(sum[:]),
		"unix_mode":  domain.NormalizeSkillFileMode(file.Mode),
		"updated_at": &now,
	})
}

// DeleteSkillFile removes one binary file and refreshes the manifest.
func (r *CatalogRepo) DeleteSkillFile(skillID, path string, expected ...string) error {
	_, err := r.RemoveSkillFile(skillID, path, firstSourceRevision(expected))
	return err
}
func (r *CatalogRepo) RemoveSkillFile(skillID, path, expected string) (domain.SkillDefinition, error) {
	var saved domain.SkillDefinition
	err := r.editSource("skills", skillID, expected, func(catalog *CatalogRepo, raw []byte, token string, exists bool) error {
		if !exists {
			return ErrSourceConflict
		}
		var skill domain.SkillDefinition
		if err := json.Unmarshal(raw, &skill); err != nil {
			return err
		}
		if skill.Source != nil {
			return ErrSkillSourceReadOnly
		}
		now := time.Now().UTC()
		skill.SourceRevision = token
		skill.UpdatedAt = now
		if err := writeSource(catalog.DB, "skills", map[string]any{"id": skill.ID, "name": skill.Name, "payload": mustJSON(skill), "updated_at": timePtr(now)}, true); err != nil {
			return err
		}
		if err := catalog.DB.Where("skill_id = ? AND path = ?", skillID, path).Delete(&skillFileRow{}).Error; err != nil {
			return err
		}
		if err := refreshSkillFileRefs(catalog.DB, skillID, now); err != nil {
			return err
		}
		saved, _ = catalog.Skill(skillID)
		return nil
	})
	return saved, err
}

// refreshSkillFileRefs rewrites the fileRefs manifest on the skill payload
// from the skill_files table, which is the source of truth.
func refreshSkillFileRefs(tx *gorm.DB, skillID string, now time.Time) error {
	skill, ok := payloadOne[domain.SkillDefinition](tx, "skills", skillID)
	if !ok {
		return fmt.Errorf("skill %s not found", skillID)
	}
	var rows []skillFileRow
	if err := tx.Where("skill_id = ?", skillID).Order("path").Find(&rows).Error; err != nil {
		return err
	}
	refs := make([]domain.SkillFileRef, 0, len(rows))
	for _, row := range rows {
		ref := domain.SkillFileRef{Path: row.Path, Size: row.Size, SHA256: row.SHA256, Mode: row.UnixMode}
		if row.Updated != nil {
			ref.UpdatedAt = *row.Updated
		}
		refs = append(refs, ref)
	}
	skill.FileRefs = refs
	skill.UpdatedAt = now
	return upsert(tx, "skills", map[string]any{
		"id": skill.ID, "name": skill.Name, "payload": mustJSON(skill), "updated_at": timePtr(now),
	})
}
