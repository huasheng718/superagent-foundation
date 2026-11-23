-- Manual operator-reviewed DDL only. No seed/backfill/startup execution.
-- Keep historical packages and decisions on rollback; disable candidate routes.
CREATE TABLE skill_candidates (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  project_id VARCHAR(64) NOT NULL,
  state VARCHAR(32) NOT NULL,
  revision INT NOT NULL,
  payload JSON NOT NULL,
  created_at DATETIME(6) NOT NULL,
  KEY idx_skill_candidate_scope (project_id, state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
