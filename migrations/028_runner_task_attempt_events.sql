-- Manual MySQL 8 transition for runner task protocol v2.
-- STOP all platform instances and Runners first; take and verify a backup.
-- Preflight: SHOW CREATE TABLE runner_task_events;
-- Required old shape: PRIMARY KEY (task_id, seq), no attempt column.
-- If AutoMigrate has already added attempt, do NOT run this script unchanged:
-- inspect the schema, retain its historical zero values and replace only the PK.
-- Fresh v2 databases already have the target shape and skip this script.
-- Old rows are deliberately attempt 0 (unknown); never infer a generation.
-- This single atomic DDL retains every event and changes its identity.
ALTER TABLE runner_task_events
    ADD COLUMN attempt BIGINT NOT NULL DEFAULT 0 AFTER task_id,
    DROP PRIMARY KEY,
    ADD PRIMARY KEY (task_id, attempt, seq);
-- Verify row counts against backup and SHOW INDEX FROM runner_task_events.
-- Deploy v2 platform and v2 Runners together. Old claims receive HTTP 426;
-- new Runners reject servers lacking the v2 response header.
-- Do not roll back the PK to (task_id, seq): v2 attempts can reuse sequence
-- numbers. Roll back binaries only after stopping work and restoring the
-- verified full pre-change backup (including task rows); never discard v2
-- history or reset generations in place.
