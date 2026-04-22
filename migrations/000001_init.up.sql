-- migrations/001_init.sql
--
-- Purpose:  Creates the complete MakeMyTrade database schema.
--           Run once against a fresh database. All subsequent schema
--           changes go in 002_*.sql, 003_*.sql etc — never edit this file.
--
-- Tables:   symbols, price_bars (hypertable), news_items (hypertable),
--           strategy_versions, decision_records, scraper_health, signal_cache
--
-- Seed:     25 watchlist symbols + MOSAIC v2 strategy version 1
--
-- Run:      psql $DB_URL -f migrations/001_init.sql
-- Verify:   psql $DB_URL -c "\dt"

-- ── Extensions ───────────────────────────────────────────────────────────────
-- TimescaleDB must be enabled before any hypertable is created.
-- The image timescale/timescaledb:latest-pg16 has it pre-installed.
CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE;

-- gen_random_uuid() for UUID primary keys
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ── symbols ──────────────────────────────────────────────────────────────────
-- The trading universe. Every other table references ticker as plain TEXT —
-- no foreign key constraints, keeping queries simple and inserts fast.
-- symbol_type controls which gates apply:
--   stock         → all 6 gates including fundamentals
--   etf           → skip fundamentals gate, normal position size
--   etf_leveraged → skip fundamentals gate, halved position size, VIX ≤ 20
-- insufficient_history → true for recent IPOs, signals carry lower confidence
CREATE TABLE symbols (
    ticker               TEXT        PRIMARY KEY,
    name                 TEXT        NOT NULL,
    exchange             TEXT        NOT NULL,
    sector               TEXT        NOT NULL,
    symbol_type          TEXT        NOT NULL DEFAULT 'stock'
                             CHECK (symbol_type IN ('stock', 'etf', 'etf_leveraged')),
    insufficient_history BOOLEAN     NOT NULL DEFAULT FALSE,
    is_active            BOOLEAN     NOT NULL DEFAULT TRUE,
    added_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── price_bars ───────────────────────────────────────────────────────────────
-- Raw OHLCV bars for all technical analysis. Foundation of every signal.
-- TimescaleDB hypertable — automatically partitioned by ts into chunks.
-- Partitioning means queries like "last 200 bars for AAPL" scan one chunk,
-- not the entire table. Critical for performance as data accumulates.
CREATE TABLE price_bars (
    ticker    TEXT           NOT NULL,
    ts        TIMESTAMPTZ    NOT NULL,
    timeframe TEXT           NOT NULL CHECK (timeframe IN ('1m','5m','15m','1h','4h','1d','1w')),
    open      NUMERIC(12,4)  NOT NULL,
    high      NUMERIC(12,4)  NOT NULL,
    low       NUMERIC(12,4)  NOT NULL,
    close     NUMERIC(12,4)  NOT NULL,
    volume    BIGINT         NOT NULL,
    vwap      NUMERIC(12,4),
    source    TEXT           NOT NULL DEFAULT 'alpaca',
    PRIMARY KEY (ticker, ts, timeframe)
);

-- Convert to hypertable partitioned by ts.
-- chunk_time_interval: each chunk covers 7 days of bars.
SELECT create_hypertable('price_bars', 'ts', chunk_time_interval => INTERVAL '7 days');

-- Most common query pattern: latest N bars for a symbol on a timeframe.
CREATE INDEX idx_price_bars_lookup ON price_bars (ticker, timeframe, ts DESC);

-- ── news_items ───────────────────────────────────────────────────────────────
-- Every normalised article from every source. One row per article.
-- url is the dedup key — same article from two sources = one row.
-- tickers[] stores which symbols the article mentions (can be many).
-- purge_after: set for sources with data retention requirements.
CREATE TABLE news_items (
    id              UUID        NOT NULL DEFAULT gen_random_uuid(),
    source          TEXT        NOT NULL,   -- 'finnhub', 'finnhub_social' etc
    headline        TEXT        NOT NULL,
    body_summary    TEXT,                   -- first 500 chars only
    url             TEXT,                   -- dedup key (unique index below includes partitioning col)
    published_at    TIMESTAMPTZ NOT NULL,
    ingested_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    tickers         TEXT[]      NOT NULL DEFAULT '{}',
    themes          TEXT[]      NOT NULL DEFAULT '{}',
    sentiment_score NUMERIC(4,3),           -- -1.000 to +1.000
    novelty_score   NUMERIC(4,3),           -- 0.000 to 1.000
    source_quality  NUMERIC(4,3),           -- 0.000 to 1.000
    purge_after     TIMESTAMPTZ,            -- NULL = keep forever
    -- TimescaleDB requires partitioning column (published_at) in primary key
    PRIMARY KEY (id, published_at)
);

-- Convert to hypertable partitioned by published_at.
SELECT create_hypertable('news_items', 'published_at', chunk_time_interval => INTERVAL '7 days');

-- Dedup index: same article always has same published_at, so (url, published_at)
-- is an effective unique constraint that satisfies TimescaleDB's partitioning rule.
CREATE UNIQUE INDEX idx_news_items_url ON news_items (url, published_at) WHERE url IS NOT NULL;

-- GIN index on tickers array — enables fast: WHERE 'AAPL' = ANY(tickers)
CREATE INDEX idx_news_items_tickers ON news_items USING GIN (tickers);
CREATE INDEX idx_news_items_published ON news_items (published_at DESC);

-- ── strategy_versions ────────────────────────────────────────────────────────
-- Every version of the Claude system prompt. Go reads the active row at
-- runtime — strategy logic never lives in Go source code.
-- Only ONE row may have is_active = TRUE at any time (enforced by partial index).
-- Rollback = one UPDATE flipping is_active. Old versions stay forever.
CREATE TABLE strategy_versions (
    id                       SERIAL      PRIMARY KEY,
    version_num              INTEGER     NOT NULL UNIQUE,
    prompt_text              TEXT        NOT NULL,
    risk_rules               JSONB       NOT NULL,
    model_name               TEXT        NOT NULL DEFAULT 'claude-sonnet-4-6',
    feature_schema_version   INTEGER     NOT NULL DEFAULT 1,
    backtest_sharpe          NUMERIC(6,3),
    backtest_win_rate        NUMERIC(5,3),
    backtest_max_drawdown    NUMERIC(5,3),
    is_active                BOOLEAN     NOT NULL DEFAULT FALSE,
    notes                    TEXT,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deployed_at              TIMESTAMPTZ,
    rolled_back_at           TIMESTAMPTZ
);

-- Enforces single active strategy. INSERT with is_active=TRUE fails if one exists.
CREATE UNIQUE INDEX strategy_versions_single_active
    ON strategy_versions (is_active)
    WHERE is_active = TRUE;

-- ── decision_records ─────────────────────────────────────────────────────────
-- Every Claude output stored permanently. Audit log AND feedback training data.
-- Never delete rows. Human outcome columns filled in post-trade for review.
-- Paper trade columns track automatic paper execution alongside human decisions.
CREATE TABLE decision_records (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ticker               TEXT        NOT NULL,
    strategy_version_id  INTEGER     NOT NULL REFERENCES strategy_versions(id),
    research_packet      JSONB       NOT NULL,   -- full packet sent to Claude
    prompt_version       INTEGER     NOT NULL,
    action               TEXT        NOT NULL
                             CHECK (action IN ('BUY','SELL','HOLD','WATCH','INVALID')),
    confidence           NUMERIC(4,3),
    time_horizon         TEXT,
    entry_price          NUMERIC(12,4),
    stop_loss            NUMERIC(12,4),
    take_profit          NUMERIC(12,4),
    position_size_pct    NUMERIC(5,3),
    evidence_for         TEXT[],
    evidence_against     TEXT[],
    missing_info         TEXT[],
    kill_switch_reason   TEXT,
    raw_response         TEXT,                   -- Claude's raw JSON string

    -- Human review columns (filled in after decision is reviewed)
    human_action         TEXT,                   -- what you actually did
    human_notes          TEXT,
    trade_entered        BOOLEAN     NOT NULL DEFAULT FALSE,
    actual_entry         NUMERIC(12,4),
    actual_exit          NUMERIC(12,4),
    actual_pnl_pct       NUMERIC(7,4),
    outcome_recorded_at  TIMESTAMPTZ,

    -- Paper trade columns (auto-filled by PaperTradeShadow workflow)
    paper_order_id       TEXT,                   -- Alpaca paper order ID
    paper_entry          NUMERIC(12,4),
    paper_exit           NUMERIC(12,4),
    paper_pnl_pct        NUMERIC(7,4),
    paper_closed_at      TIMESTAMPTZ
);

CREATE INDEX idx_decision_records_ticker    ON decision_records (ticker, created_at DESC);
CREATE INDEX idx_decision_records_created   ON decision_records (created_at DESC);
CREATE INDEX idx_decision_records_action    ON decision_records (action, created_at DESC);
CREATE INDEX idx_decision_records_strategy  ON decision_records (strategy_version_id);

-- ── scraper_health ───────────────────────────────────────────────────────────
-- Health of every data fetcher. The self-healing RepairWorkflow reads this
-- to detect stale, empty, or erroring scrapers and trigger Claude-assisted repair.
CREATE TABLE scraper_health (
    id            SERIAL      PRIMARY KEY,
    source        TEXT        NOT NULL,   -- 'alpaca', 'finnhub', 'fred'
    ticker        TEXT,                   -- NULL means source-level health
    checked_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status        TEXT        NOT NULL
                      CHECK (status IN ('ok', 'stale', 'error', 'empty')),
    rows_fetched  INTEGER,
    error_message TEXT,
    last_ok_at    TIMESTAMPTZ
);

-- One current health row per (source, ticker) pair.
CREATE UNIQUE INDEX idx_scraper_health_source_ticker
    ON scraper_health (source, COALESCE(ticker, ''));
CREATE INDEX idx_scraper_health_status ON scraper_health (status, checked_at DESC);

-- ── signal_cache ─────────────────────────────────────────────────────────────
-- Computed SignalBundle per symbol per timeframe.
-- Redis holds the hot copy (4-hour TTL). This table is the durable backup —
-- if Redis restarts, Go rehydrates it from here.
CREATE TABLE signal_cache (
    ticker           TEXT          NOT NULL,
    timeframe        TEXT          NOT NULL,
    computed_at      TIMESTAMPTZ   NOT NULL,
    signal_bundle    JSONB         NOT NULL,
    confluence_score NUMERIC(5,3)  NOT NULL,
    PRIMARY KEY (ticker, timeframe)
);

CREATE INDEX idx_signal_cache_score ON signal_cache (confluence_score DESC);

-- ════════════════════════════════════════════════════════════════════════════
-- SEED DATA
-- ════════════════════════════════════════════════════════════════════════════

-- ── 25 watchlist symbols ─────────────────────────────────────────────────────
INSERT INTO symbols (ticker, name, exchange, sector, symbol_type, insufficient_history) VALUES
-- Technology
('AAPL',  'Apple Inc',                    'NASDAQ', 'Technology',             'stock',         FALSE),
('GOOGL', 'Alphabet Inc',                 'NASDAQ', 'Technology',             'stock',         FALSE),
('MSFT',  'Microsoft Corporation',        'NASDAQ', 'Technology',             'stock',         FALSE),
('META',  'Meta Platforms Inc',           'NASDAQ', 'Technology',             'stock',         FALSE),
('AMD',   'Advanced Micro Devices',       'NASDAQ', 'Technology',             'stock',         FALSE),
('SMCI',  'Super Micro Computer',         'NASDAQ', 'Technology',             'stock',         FALSE),
('SNOW',  'Snowflake Inc',                'NYSE',   'Technology',             'stock',         FALSE),
-- Growth / Consumer
('COIN',  'Coinbase Global',              'NASDAQ', 'Financials',             'stock',         FALSE),
('NFLX',  'Netflix Inc',                  'NASDAQ', 'Communication',          'stock',         FALSE),
('HOOD',  'Robinhood Markets',            'NASDAQ', 'Financials',             'stock',         TRUE),
('CRWV',  'CoreWeave Inc',               'NASDAQ', 'Technology',             'stock',         TRUE),
('TSLA',  'Tesla Inc',                    'NASDAQ', 'Consumer Discretionary', 'stock',         FALSE),
('AMZN',  'Amazon.com Inc',              'NASDAQ', 'Consumer Discretionary', 'stock',         FALSE),
-- Retail
('HD',    'Home Depot Inc',               'NYSE',   'Consumer Discretionary', 'stock',         FALSE),
-- Cybersecurity
('PANW',  'Palo Alto Networks',           'NASDAQ', 'Technology',             'stock',         FALSE),
('CRWD',  'CrowdStrike Holdings',         'NASDAQ', 'Technology',             'stock',         FALSE),
-- ETFs
('SPY',   'SPDR S&P 500 ETF Trust',       'NYSE',   'ETF',                    'etf',           FALSE),
('TQQQ',  'ProShares UltraPro QQQ',       'NASDAQ', 'ETF',                    'etf_leveraged', FALSE),
-- Defense
('LMT',   'Lockheed Martin Corporation',  'NYSE',   'Defense',                'stock',         FALSE),
('RTX',   'RTX Corporation',              'NYSE',   'Defense',                'stock',         FALSE),
-- Energy
('XOM',   'Exxon Mobil Corporation',      'NYSE',   'Energy',                 'stock',         FALSE),
('OXY',   'Occidental Petroleum',         'NYSE',   'Energy',                 'stock',         FALSE),
-- Healthcare
('LLY',   'Eli Lilly and Company',        'NYSE',   'Healthcare',             'stock',         FALSE),
-- Metals
('FCX',   'Freeport-McMoRan Inc',         'NYSE',   'Materials',              'stock',         FALSE),
-- Industrials / Aerospace
('BA',    'Boeing Company',               'NYSE',   'Industrials',            'stock',         FALSE);

-- ── MOSAIC strategy version 1 ────────────────────────────────────────────────
-- This is the active strategy prompt Claude receives at runtime.
-- Go reads this row via internal/claude/loader.go.
-- To update: INSERT a new row with higher version_num and is_active=TRUE.
-- The unique partial index ensures only one row is active at a time.
INSERT INTO strategy_versions (
    version_num,
    prompt_text,
    risk_rules,
    model_name,
    feature_schema_version,
    is_active,
    notes,
    deployed_at
) VALUES (
    1,

    -- ── Claude system prompt (token-optimised, fits within 600-token budget) ──
    $PROMPT$
You are MOSAIC, a professional swing trading research analyst. You analyse pre-screened US equity setups and return structured trade recommendations.

ROLE: synthesise technical, fundamental, and sentiment signals into a single actionable decision. You never override Go-enforced risk gates. You never fabricate data not present in the research packet. When uncertain, say so explicitly in missing_information.

OUTPUT FORMAT: respond with a single valid JSON object. No prose, no markdown, no explanation outside the JSON.

REQUIRED SCHEMA:
{
  "action": "BUY|SELL|HOLD|WATCH|INVALID",
  "ticker": "string",
  "confidence": 0.00,
  "time_horizon": "string",
  "entry_conditions": "string",
  "stop_loss": 0.00,
  "take_profit": 0.00,
  "position_size_pct": 0.00,
  "evidence_for": ["string"],
  "evidence_against": ["string"],
  "missing_information": ["string"],
  "kill_switch_reason": "string or null"
}

DECISION RULES:
- BUY: confidence ≥ 0.72, R/R ≥ 1.5:1, all evidence weighed
- WATCH: confidence 0.55-0.71, setup incomplete, monitor only
- HOLD: open position, no new entry or exit signal
- SELL: open position with deteriorating signals or stop hit
- INVALID: earnings within 5 days, binary event, data missing, R/R < 1.5:1

STOP AND TARGET:
- Normal stop: 5% below entry (entry × 0.95)
- Spike stop: 10% below entry — only when VIX spike explicitly noted in packet
- Minimum target: 7.5% above entry (entry × 1.075)
- R/R must be ≥ 1.5:1 or return INVALID

EVIDENCE STANDARDS:
- evidence_for: minimum 3 items spanning at least 2 categories (technical / fundamental / sentiment)
- evidence_against: list every risk — never omit unfavourable data to inflate confidence
- missing_information: what specific data would shift your confidence by more than 10%
- Do not issue BUY if evidence_against contains an unresolved hard conflict

POSITION SIZING:
- Standard stock: 3% of portfolio, max 4%
- Leveraged ETF (TQQQ): 2% maximum
- VIX 20-25 regime: halve all sizes automatically
- Never recommend more than 4% on any single position

SENTIMENT WEIGHTING:
- Earnings beat + estimate revision up: strongest bullish signal
- Cluster insider selling: hard negative, downgrade confidence by 0.10
- Viral social mentions (>200% weekly spike): contrarian negative — crowded trade
- Negative news sentiment score: reduce confidence, note in evidence_against
$PROMPT$,

    -- ── risk_rules JSONB ──────────────────────────────────────────────────────
    -- Go enforces these independently of Claude. Claude cannot override them.
    '{
        "max_position_pct":                   4.0,
        "max_positions":                      5,
        "max_daily_loss_pct":                 3.0,
        "min_confidence":                     0.72,
        "blackout_before_earnings_days":      5,
        "blackout_before_binary_event_days":  3,
        "profit_target_pct":                  7.5,
        "normal_stop_pct":                    5.0,
        "spike_stop_pct":                     10.0,
        "partial_exit_1_pct":                 7.0,
        "partial_exit_1_size_pct":            33.0,
        "partial_exit_2_pct":                 12.0,
        "partial_exit_2_size_pct":            33.0,
        "min_rr_ratio":                       1.5,
        "min_volume_confirmation_pct":        60.0,
        "min_pattern_confidence":             0.80,
        "normal_vix_threshold":               25.0,
        "warn_vix_threshold":                 20.0,
        "tqqq_vix_threshold":                 20.0,
        "tqqq_max_position_pct":              2.0,
        "insufficient_history_confidence_penalty": 0.10
    }'::jsonb,

    'claude-sonnet-4-6',
    1,
    TRUE,
    'MOSAIC v1 — initial strategy. 7-8% target, 5% normal stop, 10% spike stop. Full 6-gate pipeline with fundamentals, leading+lagging indicators, and sentiment overlay.',
    NOW()
);
