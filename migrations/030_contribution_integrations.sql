-- Manually apply on the controller before enabling contribution integration.
-- Additive DDL only; no startup seed, data migration, or automatic execution.
CREATE TABLE IF NOT EXISTS contribution_integrations (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  run_id VARCHAR(64) NOT NULL,
  repository_id VARCHAR(64) NOT NULL,
  state VARCHAR(32) NOT NULL,
  payload LONGTEXT NOT NULL,
  created_at DATETIME(3) NOT NULL,
  KEY idx_contribution_integration_run (run_id),
  KEY idx_contribution_integration_repository (repository_id)
);
