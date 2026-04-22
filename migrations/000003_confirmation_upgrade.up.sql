-- migrations/000003_confirmation_upgrade.up.sql
--
-- Adds explicit lifecycle columns to trade_candidates so the confirmation
-- activity and the daily-analysis handler can route by status without
-- parsing claude_rationale JSON on every read.
--
-- candidate_status: engine-computed lifecycle state, updated by confirmation.
--   Values: rejected | structural_candidate | entry_ready |
--           blocked_by_event | confirmed | watch_only
--
-- setup_family: which of the 4 YAML families matched (e.g. bullish_continuation).
--   Needed by confirmation to pick the right signal direction (calls vs puts).
--
-- direction: 'bullish' or 'bearish', derived from setup_family at analysis time.
--   Stored separately for fast confirmation queries.
--
-- prev_day_volume: prior session's total volume, stored so confirmation can
--   compute a volume_support threshold without re-fetching daily bars.

ALTER TABLE trade_candidates
    ADD COLUMN IF NOT EXISTS candidate_status  TEXT,
    ADD COLUMN IF NOT EXISTS setup_family      TEXT,
    ADD COLUMN IF NOT EXISTS direction         TEXT,
    ADD COLUMN IF NOT EXISTS prev_day_volume   BIGINT;

CREATE INDEX IF NOT EXISTS idx_candidates_status_date
    ON trade_candidates (trade_date, candidate_status);

CREATE INDEX IF NOT EXISTS idx_candidates_family
    ON trade_candidates (trade_date, setup_family);

-- Backfill existing rows: everything without a status is treated as rejected
-- (safe because entry_ready/confirmed rows will be re-generated on next scan).
UPDATE trade_candidates
SET candidate_status = CASE
    WHEN all_gates_passed = TRUE  THEN 'structural_candidate'
    ELSE 'rejected'
END
WHERE candidate_status IS NULL;
