-- 000008_iv_professional.up.sql
--
-- Adds iv_snapshots for rolling proxy-IV rank computation.
-- Proxy IV = atm_call_ask / (underlying_price * sqrt(dte/252))
-- This is the Black-Scholes at-the-money approximation, proportional to annualised IV.
-- Rolling percentile rank over last 30 trading days tells us whether we are buying
-- cheap or expensive premium — the single most important professional-grade filter.

CREATE TABLE IF NOT EXISTS iv_snapshots (
    ticker           TEXT           NOT NULL,
    snapshot_date    DATE           NOT NULL,
    atm_symbol       TEXT,                           -- OCC symbol of the ATM contract used
    atm_strike       NUMERIC(12,4),
    underlying_price NUMERIC(12,4),
    atm_call_ask     NUMERIC(10,4),
    dte              INTEGER,
    proxy_iv         NUMERIC(10,6)  NOT NULL,        -- normalised vol estimate
    created_at       TIMESTAMPTZ    NOT NULL DEFAULT NOW(),
    PRIMARY KEY (ticker, snapshot_date)
);

CREATE INDEX IF NOT EXISTS idx_iv_snapshots_ticker_date
    ON iv_snapshots (ticker, snapshot_date DESC);
