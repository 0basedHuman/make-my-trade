-- migrations/000005_option_pnl.down.sql
ALTER TABLE paper_positions
    DROP COLUMN IF EXISTS option_symbol,
    DROP COLUMN IF EXISTS option_premium;
