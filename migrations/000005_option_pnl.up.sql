-- migrations/000005_option_pnl.up.sql
--
-- Adds option-level P&L tracking to paper_positions.
--
-- WHY:  entry_price currently stores the underlying stock price at entry,
--       which is wrong for computing option P&L. For a put: stock falling
--       = option winning, so (stock_now - stock_entry)/stock_entry gives
--       a negative number when the position is actually profitable.
--       We need to store the option premium paid and the OCC symbol so the
--       daily review can fetch the current option mid-price and compute
--       (current_option_price - premium_paid) / premium_paid * 100.
--
-- Changes:
--   option_symbol  — OCC symbol of the contract bought, e.g. RTX260508P00190000
--   option_premium — premium paid per contract in dollars, e.g. 6.58

ALTER TABLE paper_positions
    ADD COLUMN IF NOT EXISTS option_symbol  TEXT,           -- OCC contract symbol
    ADD COLUMN IF NOT EXISTS option_premium NUMERIC(12,4);  -- price paid per contract ($)
