-- migrations/000003_confirmation_upgrade.down.sql
DROP INDEX IF EXISTS idx_candidates_status_date;
DROP INDEX IF EXISTS idx_candidates_family;

ALTER TABLE trade_candidates
    DROP COLUMN IF EXISTS candidate_status,
    DROP COLUMN IF EXISTS setup_family,
    DROP COLUMN IF EXISTS direction,
    DROP COLUMN IF EXISTS prev_day_volume;
