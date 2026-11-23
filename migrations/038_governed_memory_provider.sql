-- Manual operator-reviewed SQL only. No seed, data migration or startup execution.
CREATE TABLE memory_provider_mirrors (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 project_id VARCHAR(64) NOT NULL,
 scope_sha256 VARCHAR(64) NOT NULL,
 state VARCHAR(32) NOT NULL,
 payload JSON NOT NULL,
 created_at DATETIME(6) NOT NULL,
 updated_at DATETIME(6) NOT NULL,
 KEY idx_memory_provider_scope (project_id, scope_sha256)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE TABLE memory_recall_receipts (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 project_id VARCHAR(64) NOT NULL,
 user_id BIGINT NOT NULL,
 payload JSON NOT NULL,
 created_at DATETIME(6) NOT NULL,
 KEY idx_memory_receipt_scope (project_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
