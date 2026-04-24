-- migrations/000007_risk_state.down.sql
DROP INDEX IF EXISTS idx_paper_positions_open_risk;

ALTER TABLE paper_positions
    DROP COLUMN IF EXISTS peak_option_price,
    DROP COLUMN IF EXISTS trailing_active,
    DROP COLUMN IF EXISTS last_option_price,
    DROP COLUMN IF EXISTS last_risk_check_at,
    DROP COLUMN IF EXISTS hold_overnight_approved,
    DROP COLUMN IF EXISTS hold_overnight_approved_at;
