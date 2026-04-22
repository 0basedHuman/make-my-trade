-- migrations/000004_auto_entry.up.sql
--
-- Enables autonomous paper-trade entry and weekly review persistence.
--
-- Changes:
-- 1. UNIQUE (candidate_id) on paper_positions — required so CreatePaperPosition
--    can use ON CONFLICT (candidate_id) DO UPDATE and remain idempotent across
--    Temporal retries and HTTP confirmation re-runs.
--
-- 2. option_type + setup_family columns on paper_positions — needed so the
--    daily position-review activity can send the right context to Claude
--    without re-joining back to trade_candidates every time.
--
-- 3. weekly_reviews table — stores the output of RunWeeklyReviewActivity.
--    One row per week_start (idempotent: re-running the weekly review replaces
--    the existing summary row).

-- ── 1. paper_positions: unique candidate_id ──────────────────────────────────
-- NULL candidate_id rows (manual / legacy) are exempt — Postgres unique indexes
-- allow multiple NULLs, so this does not break any existing rows.
ALTER TABLE paper_positions
    ADD CONSTRAINT paper_positions_candidate_id_unique UNIQUE (candidate_id);

-- ── 2. paper_positions: review context columns ───────────────────────────────
ALTER TABLE paper_positions
    ADD COLUMN IF NOT EXISTS option_type  TEXT,   -- 'call' | 'put'
    ADD COLUMN IF NOT EXISTS setup_family TEXT;   -- e.g. 'bullish_continuation'

-- ── 3. weekly_reviews ────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS weekly_reviews (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    week_start  DATE        NOT NULL,
    week_end    DATE        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    summary     TEXT        NOT NULL,   -- Claude's full weekly review text

    UNIQUE (week_start)
);

CREATE INDEX IF NOT EXISTS idx_weekly_reviews_start ON weekly_reviews (week_start DESC);
