-- Durable per-registration cleanup state for placed Runs.
-- Apply manually with the other baseline migrations; startup does not backfill.
CREATE TABLE IF NOT EXISTS run_workspace_cleanup_participants (
    run_id VARCHAR(64) NOT NULL,
    action VARCHAR(64) NOT NULL,
    runner_id VARCHAR(64) NOT NULL,
    registration_sha256 VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL,
    task_id VARCHAR(64) NOT NULL DEFAULT '',
    error TEXT NOT NULL,
    created_at DATETIME NULL,
    updated_at DATETIME NULL,
    completed_at DATETIME NULL,
    PRIMARY KEY (run_id, action, runner_id, registration_sha256),
    KEY idx_cleanup_participants_status (run_id, status)
);
