-- Rename gate_btc → gate_relative_strength in trade_candidates.
-- The BTC ROC gate is removed; the column now stores the relative_strength gate result.
ALTER TABLE trade_candidates
    RENAME COLUMN gate_btc TO gate_relative_strength;
