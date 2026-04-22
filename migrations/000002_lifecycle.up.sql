-- migrations/000002_lifecycle.up.sql
--
-- Adds the explicit lifecycle entities required by the new product model.
-- Separates pre-open candidates, confirmation, paper-position ledger,
-- event log, daily reviews, and no-trade-day summaries.
-- Also adds NVDA to the watchlist.

-- ── trade_candidates ─────────────────────────────────────────────────────────
-- One row per symbol per trading day that passes all hard qualifiers.
-- Built deterministically before market open from prior day's data.
-- This is NOT an audit log — it is the operational candidate list.
CREATE TABLE trade_candidates (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    trade_date          DATE        NOT NULL,
    ticker              TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Hard qualifier gate results (Layer A)
    gate_trend          BOOLEAN     NOT NULL DEFAULT FALSE,  -- EMA20>EMA100, close>EMA20
    gate_momentum       BOOLEAN     NOT NULL DEFAULT FALSE,  -- MACD hist > 0
    gate_volume         BOOLEAN     NOT NULL DEFAULT FALSE,  -- vol/avg20 > 1.2
    gate_vix            BOOLEAN     NOT NULL DEFAULT FALSE,  -- VIX < 24
    gate_btc            BOOLEAN     NOT NULL DEFAULT FALSE,  -- BTC 20d ROC > 0
    gate_rsi            BOOLEAN     NOT NULL DEFAULT FALSE,  -- RSI 50-72
    all_gates_passed    BOOLEAN     NOT NULL DEFAULT FALSE,

    -- Indicator snapshot at analysis time
    close_price         NUMERIC(12,4),
    ema20               NUMERIC(12,4),
    ema100              NUMERIC(12,4),
    rsi14               NUMERIC(6,3),
    macd_hist           NUMERIC(10,6),
    volume_ratio        NUMERIC(6,3),
    vix_level           NUMERIC(6,2),
    btc_roc20           NUMERIC(8,4),

    -- Pattern scoring (Layer B)
    pattern_name        TEXT,        -- 'bull_flag', 'tight_base', 'vcb', 'rs_leader', etc.
    pattern_score       NUMERIC(4,3),
    anti_patterns       TEXT[],      -- detected anti-patterns if any
    rejected_by_anti    BOOLEAN      NOT NULL DEFAULT FALSE,

    -- Trade parameters
    entry_low           NUMERIC(12,4),
    entry_high          NUMERIC(12,4),
    stop_loss           NUMERIC(12,4),
    target1             NUMERIC(12,4),
    target2             NUMERIC(12,4),
    rr_ratio            NUMERIC(5,2),
    hold_days_min       INTEGER,
    hold_days_base      INTEGER,
    hold_days_max       INTEGER,

    -- Claude pre-open review
    claude_action       TEXT CHECK (claude_action IN ('BUY','WATCH','INVALID','PENDING','SKIPPED')),
    claude_confidence   NUMERIC(4,3),
    claude_rationale    TEXT,
    claude_reviewed_at  TIMESTAMPTZ,

    -- Reject reason (if !all_gates_passed)
    reject_reason       TEXT,

    UNIQUE (trade_date, ticker)
);

CREATE INDEX idx_candidates_date       ON trade_candidates (trade_date DESC);
CREATE INDEX idx_candidates_ticker     ON trade_candidates (ticker, trade_date DESC);
CREATE INDEX idx_candidates_gates      ON trade_candidates (trade_date, all_gates_passed);
CREATE INDEX idx_candidates_action     ON trade_candidates (trade_date, claude_action);

-- ── trade_confirmations ──────────────────────────────────────────────────────
-- First-10-minute open confirmation result.
-- One row per candidate per day (created after 6:40 AM PT).
CREATE TABLE trade_confirmations (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    candidate_id         UUID        NOT NULL REFERENCES trade_candidates(id),
    trade_date           DATE        NOT NULL,
    ticker               TEXT        NOT NULL,
    confirmed_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Confirmation signals (Section N — at least 3 of 5)
    signal_level_holds   BOOLEAN     NOT NULL DEFAULT FALSE,  -- breakout/reclaim still holds
    signal_open_range    BOOLEAN     NOT NULL DEFAULT FALSE,  -- candle closes near high
    signal_no_rejection  BOOLEAN     NOT NULL DEFAULT FALSE,  -- no hard reversal wick
    signal_volume_ok     BOOLEAN     NOT NULL DEFAULT FALSE,  -- opening volume supportive
    signal_market_ok     BOOLEAN     NOT NULL DEFAULT FALSE,  -- SPY/QQQ not breaking down
    signals_passed       INTEGER     NOT NULL DEFAULT 0,      -- count of true signals

    -- Auto-reject triggers
    auto_rejected        BOOLEAN     NOT NULL DEFAULT FALSE,
    auto_reject_reason   TEXT,

    -- Result
    status               TEXT        NOT NULL
                             CHECK (status IN ('confirmed','rejected','watch_only')),
    revised_entry        NUMERIC(12,4),  -- revised entry if applicable
    revised_stop         NUMERIC(12,4),
    notes                TEXT,

    -- Opening bar data snapshot
    open_price           NUMERIC(12,4),
    first10_high         NUMERIC(12,4),
    first10_low          NUMERIC(12,4),
    first10_close        NUMERIC(12,4),
    first10_volume       BIGINT,

    UNIQUE (candidate_id)
);

CREATE INDEX idx_confirmations_date   ON trade_confirmations (trade_date DESC);
CREATE INDEX idx_confirmations_status ON trade_confirmations (trade_date, status);

-- ── paper_positions ──────────────────────────────────────────────────────────
-- Authoritative paper-trade ledger. Postgres is the source of truth.
-- Alpaca paper orders are optional mirrors — not the primary record.
CREATE TABLE paper_positions (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    candidate_id         UUID        REFERENCES trade_candidates(id),
    confirmation_id      UUID        REFERENCES trade_confirmations(id),
    ticker               TEXT        NOT NULL,
    opened_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    closed_at            TIMESTAMPTZ,

    -- State
    status               TEXT        NOT NULL DEFAULT 'open'
                             CHECK (status IN ('open','closed','cancelled')),

    -- Entry
    entry_price          NUMERIC(12,4) NOT NULL,
    entry_date           DATE          NOT NULL,
    shares               NUMERIC(12,4) NOT NULL,
    portfolio_value_at_entry NUMERIC(14,2),

    -- Risk parameters
    stop_loss            NUMERIC(12,4) NOT NULL,
    target1              NUMERIC(12,4),
    target2              NUMERIC(12,4),
    trailing_active      BOOLEAN       NOT NULL DEFAULT FALSE,
    trailing_stop        NUMERIC(12,4),

    -- Exit
    exit_price           NUMERIC(12,4),
    exit_date            DATE,
    exit_reason          TEXT,  -- 'stop_hit','target1','target2','trailing','review_exit','manual'

    -- Performance
    realized_pnl_pct     NUMERIC(7,4),
    realized_pnl_usd     NUMERIC(12,2),
    max_favorable_pct    NUMERIC(7,4),  -- MFE
    max_adverse_pct      NUMERIC(7,4),  -- MAE

    -- Alpaca paper mirror (optional)
    alpaca_order_id      TEXT,

    notes                TEXT
);

CREATE INDEX idx_paper_positions_status ON paper_positions (status, opened_at DESC);
CREATE INDEX idx_paper_positions_ticker ON paper_positions (ticker, opened_at DESC);

-- ── paper_position_events ────────────────────────────────────────────────────
-- Append-only lifecycle stream for paper positions.
-- Every material state change is logged here — never update, only insert.
-- Enables replay, audit, debugging, and analytics.
CREATE TABLE paper_position_events (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    position_id     UUID        NOT NULL REFERENCES paper_positions(id),
    ticker          TEXT        NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    event_type      TEXT        NOT NULL
                        CHECK (event_type IN (
                            'position_opened',
                            'stop_updated',
                            'target_reached',
                            'trailing_activated',
                            'partial_exit',
                            'position_closed',
                            'review_note',
                            'alpaca_synced'
                        )),
    price_at_event  NUMERIC(12,4),
    payload         JSONB       NOT NULL DEFAULT '{}'  -- event-specific data
);

CREATE INDEX idx_ppe_position   ON paper_position_events (position_id, occurred_at DESC);
CREATE INDEX idx_ppe_type       ON paper_position_events (event_type, occurred_at DESC);

-- ── position_reviews ─────────────────────────────────────────────────────────
-- Daily review output for held paper positions (Section P).
-- One review per open position per trading day.
CREATE TABLE position_reviews (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    position_id         UUID        NOT NULL REFERENCES paper_positions(id),
    ticker              TEXT        NOT NULL,
    review_date         DATE        NOT NULL,
    reviewed_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Snapshot at review time
    current_price       NUMERIC(12,4),
    pnl_pct_today       NUMERIC(7,4),
    days_held           INTEGER,

    -- Gate checks at review
    trend_intact        BOOLEAN,
    momentum_ok         BOOLEAN,
    anti_pattern_found  BOOLEAN,
    event_risk          BOOLEAN,

    -- Claude review output
    suggested_action    TEXT        NOT NULL
                            CHECK (suggested_action IN (
                                'HOLD',
                                'HOLD_TIGHTEN_STOP',
                                'PARTIAL_TAKE_PROFIT',
                                'EXIT',
                                'WATCH_CLOSELY'
                            )),
    action_rationale    TEXT,
    new_stop            NUMERIC(12,4),  -- if tightening stop

    -- Was the action taken?
    action_executed     BOOLEAN     NOT NULL DEFAULT FALSE,

    UNIQUE (position_id, review_date)
);

CREATE INDEX idx_reviews_date    ON position_reviews (review_date DESC);
CREATE INDEX idx_reviews_action  ON position_reviews (review_date, suggested_action);

-- ── daily_summaries ──────────────────────────────────────────────────────────
-- Per-day snapshot including no-trade-day state (Section S).
-- One row per trading day, upserted each morning.
CREATE TABLE daily_summaries (
    trade_date          DATE        PRIMARY KEY,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Regime snapshot
    vix_level           NUMERIC(6,2),
    btc_roc20           NUMERIC(8,4),
    spy_trend_ok        BOOLEAN,
    regime_label        TEXT,  -- 'risk_on','risk_off','elevated_vol','neutral'

    -- Analysis outcome
    symbols_scanned     INTEGER     NOT NULL DEFAULT 0,
    candidates_found    INTEGER     NOT NULL DEFAULT 0,
    candidates_confirmed INTEGER    NOT NULL DEFAULT 0,
    no_trade_today      BOOLEAN     NOT NULL DEFAULT FALSE,

    -- Explanation for no-trade days
    no_trade_reason     TEXT,
    regime_summary      TEXT,

    -- Watch list (not confirmed but watching)
    watch_tickers       TEXT[]      NOT NULL DEFAULT '{}',

    -- Daily stats
    open_positions      INTEGER     NOT NULL DEFAULT 0,
    closed_today        INTEGER     NOT NULL DEFAULT 0,
    analysis_completed  BOOLEAN     NOT NULL DEFAULT FALSE,
    analysis_completed_at TIMESTAMPTZ
);

-- ── Add NVDA ─────────────────────────────────────────────────────────────────
INSERT INTO symbols (ticker, name, exchange, sector, symbol_type, insufficient_history)
VALUES ('NVDA', 'NVIDIA Corporation', 'NASDAQ', 'Technology', 'stock', FALSE)
ON CONFLICT (ticker) DO UPDATE SET is_active = TRUE;

-- ── Update strategy to v2 (deterministic rule sheet) ────────────────────────
-- Insert new strategy version with cleaner qualifiers aligned to Section J-N
UPDATE strategy_versions SET is_active = FALSE WHERE is_active = TRUE;

INSERT INTO strategy_versions (
    version_num, prompt_text, risk_rules, model_name,
    feature_schema_version, is_active, notes, deployed_at
) VALUES (
    2,
    $PROMPT$
You are MakeMyTrade's pre-open trade reviewer. You receive a structured JSON research packet for a single US equity that has already passed all hard deterministic qualifiers. Your job is to review the evidence, weigh it, and return a single structured recommendation.

ROLE: Bounded reviewer operating on code-computed evidence. You do NOT invent indicators, override qualifiers, or freestyle new strategy rules. You synthesise the structured packet into a human-readable recommendation with explicit reasoning.

RESPONSE FORMAT: Single valid JSON object only. No prose outside the JSON.

REQUIRED SCHEMA:
{
  "action": "BUY|WATCH|INVALID",
  "ticker": "string",
  "confidence": 0.00,
  "time_horizon": "string",
  "entry_note": "string",
  "stop_note": "string",
  "evidence_for": ["string"],
  "evidence_against": ["string"],
  "key_risk": "string",
  "kill_switch_reason": null
}

DECISION RULES:
- BUY: confidence >= 0.72, R/R >= 1.5, evidence_for covers trend + momentum + at least one more
- WATCH: confidence 0.55-0.71, setup not fully ripe or one conflicting signal
- INVALID: R/R < 1.5, earnings within 5 days, critical data missing, or hard conflict in evidence

EVIDENCE STANDARDS:
- evidence_for: minimum 3 items, must span at least 2 categories (technical / sentiment / pattern)
- evidence_against: list every risk — never suppress unfavourable data
- Do not issue BUY if evidence_against contains an unresolved structural conflict
$PROMPT$,
    '{
        "stop_pct": 7.0,
        "partial1_pct": 15.0,
        "partial1_size_pct": 25.0,
        "partial2_pct": 22.0,
        "partial2_size_pct": 25.0,
        "trail_ema_days": 8,
        "max_position_pct": 8.0,
        "position_risk_pct": 2.5,
        "max_positions": 6,
        "min_confidence": 0.72,
        "min_rr": 1.5,
        "vix_hard_limit": 24.0,
        "vix_warn": 20.0,
        "btc_roc_days": 20,
        "volume_ratio_min": 1.2,
        "rsi_min": 50,
        "rsi_max": 72,
        "blackout_earnings_days": 5
    }'::jsonb,
    'claude-sonnet-4-6',
    2,
    TRUE,
    'v2 — deterministic qualifiers (Section J), pattern scoring (K), anti-pattern rejection (L), proper exit rules: 7% stop / trail 8EMA / partials at 15%+22%',
    NOW()
);
