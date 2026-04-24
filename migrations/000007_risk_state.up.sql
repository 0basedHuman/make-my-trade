-- migrations/000007_risk_state.up.sql
--
-- WHAT: Adds mechanical-exit risk-state columns to paper_positions.
--
-- WHY:  The mechanical risk manager (RunMechanicalRiskCheckActivity) needs to
--       track the option's high-water mark (peak_option_price) for trailing
--       stop logic, the current price for momentum, whether the trailing stop
--       is active, and whether Claude has approved holding overnight.
--
--       These are mutable per-position state — not derivable from history.
--
-- SAFE: All columns are nullable or have safe defaults.
--       Existing open positions start with trailing_active=false,
--       hold_overnight_approved=false, and NULLs for price tracking fields.

ALTER TABLE paper_positions
    ADD COLUMN IF NOT EXISTS peak_option_price          NUMERIC(12,4),           -- highest mid-price seen since entry
    ADD COLUMN IF NOT EXISTS trailing_active            BOOLEAN NOT NULL DEFAULT false,  -- true once +35% reached
    ADD COLUMN IF NOT EXISTS last_option_price          NUMERIC(12,4),           -- last fetched mid-price
    ADD COLUMN IF NOT EXISTS last_risk_check_at         TIMESTAMPTZ,             -- when risk check last ran for this position
    ADD COLUMN IF NOT EXISTS hold_overnight_approved    BOOLEAN NOT NULL DEFAULT false,  -- Claude approved hold overnight
    ADD COLUMN IF NOT EXISTS hold_overnight_approved_at TIMESTAMPTZ;             -- when approval was granted

-- Index for quick lookup of positions that need risk checking
CREATE INDEX IF NOT EXISTS idx_paper_positions_open_risk
    ON paper_positions (status, last_risk_check_at)
    WHERE status = 'open';
