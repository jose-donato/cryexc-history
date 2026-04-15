# cryexc-history

Self-hosted BYOD (Bring Your Own Data) history server for the [Cryexc](https://cryexc.josedonato.com) trading terminal. Streams live trades + liquidations from Binance Futures, persists to DuckDB, and serves the v1 protocol the frontend expects for footprint/DOM/liq backfill.

Point Cryexc's `Custom` history source at this server and you get the full footprint/delta/liquidation experience for whichever symbol you configure — no hosted-backend subscription required.

## Status

Phase 4 of the BYOD rollout. Single symbol, single exchange (`binancef`). Multi-symbol ingestion lands in v1.1.

## Quick start

```bash
# Build
go build -o cryexc-history ./cmd/server

# Run — defaults to BTCUSDT on Binance Futures, loopback-only, no auth
./cryexc-history

# Or pick a different symbol
./cryexc-history --symbols ETHUSDT --db-path /tmp/eth.duckdb
```

Then in the Cryexc frontend:

1. Open **Settings → History source** (non-premium; premium users are locked to the hosted backend).
2. Select **Custom (BYOD)**.
3. Enter `http://localhost:8080` as the URL.
4. Click **Test connection**. When it succeeds, the switch commits and the footprint loads from your local server.

## Flags

| Flag | Default | Notes |
|------|---------|-------|
| `--bind` | `127.0.0.1` | Interface to bind to. Leave as loopback for single-user setups. Only set to `0.0.0.0` if you actually want remote access — and set `--auth-token` when you do. |
| `--port` | `8080` | HTTP port. |
| `--db-path` | `cryexc-history.duckdb` | DuckDB file path. Will be created if missing. |
| `--auth-token` | *(empty)* | Bearer token for `/v1/*`. Empty = auth disabled. |
| `--db-memory-limit` | `256MB` | DuckDB `memory_limit` PRAGMA. |
| `--exchange` | `binancef` | Upstream exchange. Only `binancef` is wired up today. |
| `--symbols` | `BTCUSDT` | Comma-separated list. Only the first entry is ingested until v1.1. |

## Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/health` | No | Liveness probe. Returns `ok`. |
| GET | `/v1/info` | Yes | Discovery — advertises the configured exchange/symbol. |
| GET | `/v1/trades` | Yes | `?exchange=&symbol=&start_ms=&end_ms=&limit=` — trades in range. |
| GET | `/v1/liquidations` | Yes | Same params as trades. |

All `/v1/*` endpoints require `Authorization: Bearer <token>` when `--auth-token` is set. Protocol details live in the [byod-spec.md](https://github.com/jose-donato/cryexc-extension2/blob/main/docs/byod-spec.md).

## Security posture

- **Loopback + no auth** — fine for a single user running the binary on their own machine. Default.
- **Public interface + auth token** — fine. You know what you're doing.
- **Public interface + no auth** — the startup banner will yell at you. Don't do this.

CORS is deliberately permissive (reflects the request origin). A Go binary that's already exposing raw market data to any local client is not the layer to enforce browser origin policy. If you need stricter origin handling, run this behind a reverse proxy.

## Known limitations

- **No gap-fill on WS reconnect.** If the WebSocket drops and reconnects, trades that happened during the outage are not backfilled from Binance's REST `aggTrades` endpoint. For day-trading use cases on a reliable connection this is usually a non-issue; for long-running server deployments, expect occasional ~seconds-to-minutes gaps after reconnects. (See issue for eventual fix.)
- **Single symbol.** Binary ingests exactly one symbol per run. Run multiple instances on different ports + different DB paths for multi-symbol setups until v1.1 lands.
- **24h retention.** Matches the frontend's longest lookback window. Rows older than 24h are pruned every 5 minutes.
- **Binance Futures only.** Spot and other venues are trivial additions but not wired up yet.

## Schema

DuckDB tables:

```sql
CREATE TABLE trades (
    trade_id BIGINT,
    price DOUBLE,
    quantity DOUBLE,
    timestamp_ms BIGINT,
    is_buyer_maker BOOLEAN,
    symbol VARCHAR,
    exchange VARCHAR
);
CREATE INDEX idx_trades_symbol_ts ON trades(symbol, timestamp_ms);

CREATE TABLE liquidations (
    side VARCHAR,
    price DOUBLE,
    quantity DOUBLE,
    timestamp_ms BIGINT,
    notional_usd DOUBLE,
    symbol VARCHAR,
    exchange VARCHAR
);
CREATE INDEX idx_liquidations_symbol_ts ON liquidations(symbol, timestamp_ms);
```

Range queries are indexed on `(symbol, timestamp_ms)` so lookup cost stays flat as the DB grows.

## Releases

Binaries are built locally and uploaded manually to the GitHub Releases page. No CI. If you want a build for a platform that isn't up there, open an issue and tell me what you're on.

## Origins

Extracted from [cryexc-extension2](https://github.com/jose-donato/cryexc-extension2) during the BYOD history protocol v1 rollout. The hosted backend inside that monorepo still speaks the same v1 protocol for premium users — this repo is the standalone equivalent non-premium users can run themselves.
