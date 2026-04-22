-- migrations/000002_lifecycle.down.sql
-- Reverses 000002_lifecycle.up.sql

DELETE FROM strategy_versions WHERE version_num = 2;
UPDATE strategy_versions SET is_active = TRUE WHERE version_num = 1;
DELETE FROM symbols WHERE ticker = 'NVDA';

DROP TABLE IF EXISTS daily_summaries;
DROP TABLE IF EXISTS position_reviews;
DROP TABLE IF EXISTS paper_position_events;
DROP TABLE IF EXISTS paper_positions;
DROP TABLE IF EXISTS trade_confirmations;
DROP TABLE IF EXISTS trade_candidates;
