-- Manually apply to existing MySQL deployments after reviewing the schema.
-- No seed or data rewrite is needed. Incremental AutoMigrate can add this
-- new table on fresh installs; never store contributions in preview_blobs.
CREATE TABLE IF NOT EXISTS runner_contribution_blobs (
    sha256 VARCHAR(64) NOT NULL,
    content LONGBLOB NOT NULL,
    size BIGINT NOT NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (sha256),
    CONSTRAINT chk_runner_contribution_blob_size CHECK (size > 0 AND size <= 16777216)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
-- The application verifies exact SHA256/size before insert and enforces the
-- fixed 16 MiB cap. Hash IDs are immutable. Preview expiry never deletes them.
-- Confirm max_allowed_packet safely exceeds 16 MiB plus protocol overhead.
