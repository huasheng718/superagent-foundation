-- Manually apply on the controller before enabling controlled PR publication.
-- One DeliveryRepo-owned gate/outbox row per immutable integration (SQL030).
CREATE TABLE IF NOT EXISTS contribution_publications (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  run_id VARCHAR(64) NOT NULL,
  repository_id VARCHAR(64) NOT NULL,
  state VARCHAR(32) NOT NULL,
  attempt INT NOT NULL DEFAULT 0,
  lease_owner VARCHAR(128) NOT NULL DEFAULT '',
  lease_until DATETIME(6) NULL,
  payload LONGTEXT NOT NULL,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  KEY idx_contribution_publication_run (run_id),
  KEY idx_contribution_publication_repository (repository_id),
  KEY idx_contribution_publication_state (state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
-- prepared integration identity/target/gates are immutable. Only the fenced
-- publisher advances publishing -> pr_ready; this table never records merged
-- or business-done and is not fed by Runner-controlled gate callbacks.
