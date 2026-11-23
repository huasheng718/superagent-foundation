-- 032: additive membership index for node-placement admission.
-- Apply manually after inspecting 031. Do not backfill legacy Runs and do not
-- add startup data migration. Legacy rows retain runs.quota_bucket; new
-- placed Runs populate this table in the same Run insert transaction.
CREATE TABLE IF NOT EXISTS run_admission_memberships (
  run_id VARCHAR(64) NOT NULL,
  bucket VARCHAR(128) NOT NULL,
  PRIMARY KEY (run_id, bucket),
  KEY idx_run_admission_membership_bucket (bucket, run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
