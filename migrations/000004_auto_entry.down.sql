-- migrations/000004_auto_entry.down.sql

DROP TABLE IF EXISTS weekly_reviews;

ALTER TABLE paper_positions
    DROP COLUMN IF EXISTS option_type,
    DROP COLUMN IF EXISTS setup_family;

ALTER TABLE paper_positions
    DROP CONSTRAINT IF EXISTS paper_positions_candidate_id_unique;
