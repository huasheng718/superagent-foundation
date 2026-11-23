-- Manual operator-reviewed SQL only. No seed, backfill or startup execution.
-- Apply once to existing MySQL installations. Reverting drops audit history;
-- disable TeamContext selection instead when a reversible rollback is needed.
CREATE TABLE team_releases (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  project_id VARCHAR(64) NOT NULL,
  version INT NOT NULL,
  revision INT NOT NULL,
  state VARCHAR(32) NOT NULL,
  payload JSON NOT NULL,
  created_at DATETIME(6) NOT NULL,
  UNIQUE KEY uq_team_release_version (project_id, version),
  KEY idx_team_release_project (project_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE TABLE governed_memories (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  project_id VARCHAR(64) NOT NULL,
  state VARCHAR(32) NOT NULL,
  revision INT NOT NULL,
  payload JSON NOT NULL,
  created_at DATETIME(6) NOT NULL,
  KEY idx_governed_memory_scope (project_id, state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
ALTER TABLE runs ADD COLUMN intelligence JSON NULL;
