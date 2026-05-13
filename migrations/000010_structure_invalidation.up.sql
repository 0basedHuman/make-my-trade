-- Migration 000010: add structure_invalidation_level to paper_positions.
-- This stores the pattern-derived price level that voids a trade's structure.
-- For bullish: exit if underlying < structure_invalidation_level.
-- For bearish: exit if underlying > structure_invalidation_level.
-- 0 means no pattern-derived level; risk engine falls back to entry_price.
ALTER TABLE paper_positions
    ADD COLUMN IF NOT EXISTS structure_invalidation_level NUMERIC(12,4) DEFAULT 0;
