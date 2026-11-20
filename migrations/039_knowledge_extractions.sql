-- Manual operator-reviewed SQL only. No seed, data rewrite or startup execution.
CREATE TABLE knowledge_extractions (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 project_id VARCHAR(64) NOT NULL,
 payload JSON NOT NULL,
 created_at DATETIME(6) NOT NULL,
 KEY idx_knowledge_extraction_project (project_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
