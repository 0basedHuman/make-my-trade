-- migrations/000001_init.down.sql
--
-- Purpose:  Tears down everything created by 000001_init.up.sql.
--           Drops tables in reverse dependency order.
--           Run by golang-migrate when rolling back version 1.
--
-- WARNING:  This destroys all data. Only use in development.

DROP TABLE IF EXISTS signal_cache;
DROP TABLE IF EXISTS scraper_health;
DROP TABLE IF EXISTS decision_records;
DROP TABLE IF EXISTS strategy_versions;
DROP TABLE IF EXISTS news_items;
DROP TABLE IF EXISTS price_bars;
DROP TABLE IF EXISTS symbols;
DROP EXTENSION IF EXISTS pgcrypto;
DROP EXTENSION IF EXISTS timescaledb CASCADE;
