-- 000006_position_enrichment.down.sql
DROP INDEX IF EXISTS idx_paper_positions_exit_order;

ALTER TABLE paper_positions
    DROP COLUMN IF EXISTS strike,
    DROP COLUMN IF EXISTS expiration,
    DROP COLUMN IF EXISTS dte_at_entry,
    DROP COLUMN IF EXISTS exit_order_id,
    DROP COLUMN IF EXISTS exit_order_status,
    DROP COLUMN IF EXISTS claude_confirm_confidence,
    DROP COLUMN IF EXISTS claude_confirm_reason;
