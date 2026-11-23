-- Apply manually before enabling bounded execution on an existing deployment.
-- No backfill: NULL preserves the existing behavior of unbudgeted Runs.
ALTER TABLE runs ADD COLUMN execution_budget JSON NULL;
