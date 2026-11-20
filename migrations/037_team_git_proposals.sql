-- Manually apply before enabling the TeamAI proposal writer. No data seed.
ALTER TABLE contribution_publications MODIFY COLUMN run_id VARCHAR(64) NULL;
CREATE TABLE IF NOT EXISTS team_git_proposals (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  project_id VARCHAR(64) NOT NULL,
  repository_id VARCHAR(64) NOT NULL,
  state VARCHAR(32) NOT NULL,
  revision INT NOT NULL,
  payload LONGTEXT NOT NULL,
  metadata LONGTEXT NOT NULL,
  bundle LONGBLOB NOT NULL,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  KEY idx_team_git_proposals_project (project_id),
  KEY idx_team_git_proposals_repository (repository_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
-- Legacy Run publications retain their real run_id and JSON interpretation.
-- Rollback disables new write entrypoints and retains proposal/decision history.
