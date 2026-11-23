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

func (r *CatalogRepo) MCPFileRefs(mcpID string) []domain.MCPFileRef {
	var rows []mcpFileRow
	if err := r.DB.Where("mcp_id = ?", mcpID).Order("path").Find(&rows).Error; err != nil {
		return nil
	}
	out := make([]domain.MCPFileRef, 0, len(rows))
	for _, row := range rows {
		ref := domain.MCPFileRef{Path: row.Path, Size: row.Size, SHA256: row.SHA256, Mode: row.UnixMode}
		if row.Updated != nil {
			ref.UpdatedAt = *row.Updated
		}
		out = append(out, ref)
	}
	return out
}

func (r *CatalogRepo) MCPFileContent(mcpID, filePath string) ([]byte, bool) {
	var row mcpFileRow
	if err := r.DB.Where("mcp_id = ? AND path = ?", mcpID, filePath).Take(&row).Error; err != nil {
		return nil, false
	}
	return row.Content, true
}

func (r *CatalogRepo) MCPFileContents(mcpID string) map[string][]byte {
	var rows []mcpFileRow
	if err := r.DB.Where("mcp_id = ?", mcpID).Find(&rows).Error; err != nil {
		return nil
	}
	out := make(map[string][]byte, len(rows))
	for _, row := range rows {
		out[row.Path] = row.Content
	}
	return out
}

// PutMCPFiles stores one complete uploaded stdio MCP source tree.
func (r *CatalogRepo) PutMCPFiles(mcpID string, files []SkillFileInput, expected ...string) error {
	_, err := r.SaveMCPFiles(mcpID, files, firstSourceRevision(expected))
	return err
}
func (r *CatalogRepo) SaveMCPFiles(mcpID string, files []SkillFileInput, expected string) (domain.MCPServerDefinition, error) {
	var saved domain.MCPServerDefinition
	if mcpID == "" || len(files) == 0 {
		return saved, errors.New("MCP id and files are required")
	}
	err := r.editSource("mcp_servers", mcpID, expected, func(catalog *CatalogRepo, raw []byte, token string, exists bool) error {
		if !exists {
			return ErrSourceConflict
		}
		var server domain.MCPServerDefinition
		if err := json.Unmarshal(raw, &server); err != nil {
			return err
		}
		if server.MarketID != "" {
			return errors.New("market MCP files are read-only")
		}
		now := time.Now().UTC()
		server.SourceRevision = token
		server.UpdatedAt = now
		if err := writeSource(catalog.DB, "mcp_servers", map[string]any{"id": server.ID, "name": server.Name, "payload": mustJSON(server), "updated_at": timePtr(now)}, true); err != nil {
			return err
		}
		if err := catalog.DB.Where("mcp_id = ?", mcpID).Delete(&mcpFileRow{}).Error; err != nil {
			return err
		}
		for _, file := range files {
			if err := domain.ValidateRelativeFilePath(file.Path); err != nil {
				return err
			}
			if int64(len(file.Content)) > domain.MCPFileMaxBytes {
				return fmt.Errorf("MCP file exceeds maximum size")
			}
			sum := sha256.Sum256(file.Content)
			if err := upsert(catalog.DB, "mcp_files", map[string]any{"mcp_id": mcpID, "path": file.Path, "content": file.Content, "size": int64(len(file.Content)), "sha256": hex.EncodeToString(sum[:]), "unix_mode": domain.NormalizeSkillFileMode(file.Mode), "updated_at": &now}); err != nil {
				return err
			}
		}
		if err := refreshMCPFileRefs(catalog.DB, mcpID, now); err != nil {
			return err
		}
		saved, _ = catalog.MCPServer(mcpID)
		return nil
	})
	return saved, err
}
func (r *CatalogRepo) DeleteMCPFile(mcpID, path string, expected ...string) error {
	_, err := r.RemoveMCPFile(mcpID, path, firstSourceRevision(expected))
	return err
}
func (r *CatalogRepo) RemoveMCPFile(mcpID, path, expected string) (domain.MCPServerDefinition, error) {
	var saved domain.MCPServerDefinition
	err := r.editSource("mcp_servers", mcpID, expected, func(catalog *CatalogRepo, raw []byte, token string, exists bool) error {
		if !exists {
			return ErrSourceConflict
		}
		var server domain.MCPServerDefinition
		if err := json.Unmarshal(raw, &server); err != nil {
			return err
		}
		if server.MarketID != "" {
			return errors.New("market MCP files are read-only")
		}
		now := time.Now().UTC()
		server.SourceRevision = token
		server.UpdatedAt = now
		if err := writeSource(catalog.DB, "mcp_servers", map[string]any{"id": server.ID, "name": server.Name, "payload": mustJSON(server), "updated_at": timePtr(now)}, true); err != nil {
			return err
		}
		if err := catalog.DB.Where("mcp_id = ? AND path = ?", mcpID, path).Delete(&mcpFileRow{}).Error; err != nil {
			return err
		}
		if err := refreshMCPFileRefs(catalog.DB, mcpID, now); err != nil {
			return err
		}
		saved, _ = catalog.MCPServer(mcpID)
		return nil
	})
	return saved, err
}

func refreshMCPFileRefs(tx *gorm.DB, mcpID string, now time.Time) error {
	server, ok := payloadOne[domain.MCPServerDefinition](tx, "mcp_servers", mcpID)
	if !ok {
		return fmt.Errorf("MCP %s not found", mcpID)
	}
	var rows []mcpFileRow
	if err := tx.Where("mcp_id = ?", mcpID).Order("path").Find(&rows).Error; err != nil {
		return err
	}
	refs := make([]domain.MCPFileRef, 0, len(rows))
	for _, row := range rows {
		ref := domain.MCPFileRef{Path: row.Path, Size: row.Size, SHA256: row.SHA256, Mode: row.UnixMode}
		if row.Updated != nil {
			ref.UpdatedAt = *row.Updated
		}
		refs = append(refs, ref)
	}
	server.FileRefs, server.UpdatedAt = refs, now
	return upsert(tx, "mcp_servers", map[string]any{"id": server.ID, "name": server.Name, "payload": mustJSON(server), "updated_at": timePtr(now)})
}
