-- 000006_position_enrichment.up.sql
--
-- WHAT: Adds enrichment columns to paper_positions for better position tracking.
--
-- WHY:  v7 entry confirmation now records the specific option contract selected
--       by Claude. These columns allow the position review to reference the
--       exact contract without re-fetching the chain, and track the exit order
--       state separately from the position state.
--
-- SAFE: All columns are nullable (ALTER TABLE ADD COLUMN ... NULL) so existing
--       rows continue to work without migration-time backfill.

ALTER TABLE paper_positions
    ADD COLUMN IF NOT EXISTS strike         NUMERIC(12,4),
    ADD COLUMN IF NOT EXISTS expiration     DATE,
    ADD COLUMN IF NOT EXISTS dte_at_entry   INTEGER,
    ADD COLUMN IF NOT EXISTS exit_order_id  TEXT,
    ADD COLUMN IF NOT EXISTS exit_order_status TEXT DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS claude_confirm_confidence NUMERIC(5,4),
    ADD COLUMN IF NOT EXISTS claude_confirm_reason     TEXT;

-- Index for quick lookup of pending exit orders
CREATE INDEX IF NOT EXISTS idx_paper_positions_exit_order
    ON paper_positions (exit_order_id)
    WHERE exit_order_id IS NOT NULL;
