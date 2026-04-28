# cryexc-history

Bring historical data to [cryexc](https://cryexc.josedonato.com) trading terminal. Streams live trades + liquidations from Binance Futures, persists to DuckDB, and serves the v1 protocol the frontend expects for footprint/DOM/liq backfill.

Point Cryexc's `Custom` history source at this server and you get the full footprint/delta/liquidation experience for whichever symbol you configure — no hosted-backend subscription required.

## Status

Single symbol, single exchange (`binancef`).

> **Regional access.** Binance Futures is geo-blocked in several jurisdictions (US, UK, Canada, and others — check Binance's own restricted-countries list). If you're in one of those regions, the Binance WebSocket endpoints this server connects to may refuse the handshake or return empty streams. In that case you have two options: run the binary through a VPN terminating in an allowed region, or fork this repo and swap `internal/binance` / `internal/binanceliq` for a [Hyperliquid](https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/websocket) public WebSocket client — Hyperliquid is permissionless, has no geo restrictions, and exposes equivalent trade stream (hyperliquid public websocket doesn't have liquidations).

## How it works

Three-stage pipeline in a single process:

1. **Ingest** — two WebSocket clients connect to Binance Futures: one to the combined `<symbol>@trade` / `<symbol>@depth20@100ms` stream for trades + top-of-book, and one to `<symbol>@forceOrder` for liquidations. Both auto-reconnect on drop and surface their connection state through the stats package.
2. **Persist** — trades and liquidations land in DuckDB via batched inserts. Schema is indexed on `(symbol, timestamp_ms)` so range lookups stay flat as the file grows. A pruner runs every 5 minutes and deletes rows older than 24 hours (matching the frontend's longest lookback window).
3. **Serve** — `net/http` exposes `/v1/info`, `/v1/trades`, `/v1/liquidations`, and `/health`. The v1 handlers speak the BYOD protocol from [history-spec.md](https://cryexc.josedonato.com/history-spec.md) — every response field name is pinned so the frontend treats self-hosted and hosted servers as interchangeable.

All three stages live in the same Go binary, share the same DuckDB handle, and shut down together on SIGTERM/SIGINT. No external services, no message broker, no Redis — the bet is that for a single-user self-hosted deployment, "one process, one file" is better than "correct" distributed-systems plumbing.

## Install

### Option 1: prebuilt binary (recommended)

Download the asset for your platform from the [v0.1.1 release](https://github.com/jose-donato/cryexc-history/releases/tag/v0.1.1):

| Platform | Asset |
|----------|-------|
| Linux x86_64 | `cryexc-history-linux-amd64` |
| Linux arm64 | `cryexc-history-linux-arm64` |
| macOS Intel | `cryexc-history-darwin-amd64` |
| macOS Apple Silicon | `cryexc-history-darwin-arm64` |
| Windows x86_64 | `cryexc-history-windows-amd64.exe` |

Linux:

```bash
curl -LO https://github.com/jose-donato/cryexc-history/releases/download/v0.1.1/cryexc-history-linux-amd64
chmod +x cryexc-history-linux-amd64
mv cryexc-history-linux-amd64 /usr/local/bin/cryexc-history   # optional — or run from cwd
```

Binaries are **not code-signed**. On macOS the first run will be blocked by Gatekeeper — strip the quarantine attribute once and you're done:

```bash
xattr -d com.apple.quarantine cryexc-history-darwin-arm64
```

On Windows, SmartScreen may warn on first run; click **More info → Run anyway**.

### Option 2: build from source

Requires Go 1.23+ and a C toolchain (DuckDB ships as CGO).

```bash
git clone https://github.com/jose-donato/cryexc-history.git
cd cryexc-history
go build -o cryexc-history ./cmd/server
```

The resulting binary is ~52MB — most of that is DuckDB's C bindings statically linked in.

## Quick start

Default — BTCUSDT on Binance Futures, loopback only, no auth:

```bash
./cryexc-history
```

Different symbol + custom DB path:

```bash
./cryexc-history --symbols ETHUSDT --db-path /tmp/eth.duckdb
```

Expose on LAN with a token:

```bash
./cryexc-history --bind 0.0.0.0 --auth-token $(openssl rand -hex 32)
```

Then in the Cryexc frontend:

1. Open the Connection popup → **History source**.
2. Select **Custom (BYOD)**.
3. Enter `http://localhost:8080` (or your `--bind`:`--port`).
4. Paste the auth token if you set one.
5. Click **Test connection**. When it succeeds, the switch commits and the footprint history loads from your local server.

<img width="547" height="307" alt="image" src="https://github.com/user-attachments/assets/772e2de8-5b62-4373-afc3-d1b2ff93e11e" />

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
| `--version` | | Print version and exit. |

## Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/health` | No | Liveness probe. Returns `ok`. |
| GET | `/v1/info` | Yes | Discovery — advertises the configured exchange/symbol. |
| GET | `/v1/trades` | Yes | `?exchange=&symbol=&start_ms=&end_ms=&limit=` — trades in range. |
| GET | `/v1/liquidations` | Yes | Same params as trades. |

All `/v1/*` endpoints require `Authorization: Bearer <token>` when `--auth-token` is set. Full protocol details live in [history-spec.md](https://cryexc.josedonato.com/history-spec.md).

## Example requests

All examples assume the default loopback bind and no auth token. Add `-H "Authorization: Bearer <token>"` if you set `--auth-token`.

### Health check

```bash
curl http://127.0.0.1:8080/health
# ok
```

### Discovery

```bash
curl http://127.0.0.1:8080/v1/info | jq
```

```json
{
  "protocol_version": 1,
  "server": "cryexc-history/0.1.1",
  "server_time_ms": 1744723200123,
  "exchanges": [
    {
      "id": "binancef",
      "display_name": "Binance Futures",
      "symbols": [
        {
          "symbol": "BTCUSDT",
          "has_trades": true,
          "has_liquidations": true,
          "data_start_ms": 1744723000000,
          "data_end_ms": 1744723200000
        }
      ]
    }
  ]
}
```

Each symbol entry advertises whether trades/liquidations have been ingested and the span currently in the DB — the frontend uses these to decide whether it has enough history to render the requested lookback window.

### Trades in a range

```bash
NOW=$(date +%s%3N)
START=$((NOW - 60000))
curl "http://127.0.0.1:8080/v1/trades?exchange=binancef&symbol=BTCUSDT&start_ms=$START&end_ms=$NOW" | jq
```

```json
{
  "trades": [
    {
      "trade_id": 5728441234,
      "price": 64821.5,
      "quantity": 0.015,
      "timestamp_ms": 1744723140523,
      "is_buyer_maker": false
    }
  ],
  "truncated": false,
  "actual_start_ms": 1744723140123,
  "actual_end_ms": 1744723200087
}
```

`truncated: true` means the server capped the result set (most-recent N rows). Either widen the window or narrow the query.

### Liquidations in a range

```bash
curl "http://127.0.0.1:8080/v1/liquidations?exchange=binancef&symbol=BTCUSDT&start_ms=$((NOW - 3600000))&end_ms=$NOW" | jq
```

```json
{
  "liquidations": [
    {
      "side": "long",
      "price": 64750.2,
      "quantity": 0.512,
      "timestamp_ms": 1744721345671,
      "notional_usd": 33152.1
    }
  ],
  "truncated": false,
  "actual_start_ms": 1744719600000,
  "actual_end_ms": 1744723200087
}
```

## Security posture

- **Loopback + no auth** — fine for a single user running the binary on their own machine. Default.
- **Public interface + auth token** — fine. You know what you're doing.
- **Public interface + no auth** — the startup banner will yell at you. Don't do this.

CORS is deliberately permissive (reflects the request origin). A Go binary that's already exposing raw market data to any local client is not the layer to enforce browser origin policy. If you need stricter origin handling, run this behind a reverse proxy.

## Troubleshooting

**`address already in use`** — another process is on port 8080. Either stop it or run with `--port 8089` (any free port works).

**`permission denied` on `--db-path`** — the user running the binary needs write access to the directory. The default (`cryexc-history.duckdb` in the current working directory) usually works; `/tmp/` or `$HOME/` are safe alternatives. System directories like `/var/lib/` need either sudo or a pre-created directory with the right owner.

**Frontend shows "BYOD URL not configured"** — the Custom source has an empty URL in settings. Open the Connection popup, enter the URL (e.g. `http://localhost:8080`), click **Test connection**, then the switch commits.

**Frontend shows "Remote server: 401 Unauthorized"** — you set `--auth-token` on the server but didn't paste it (or pasted a different value) in the frontend's BYOD token field.

**Footprint has visible gaps right after startup** — expected. The server only persists trades it has received live. Wait a minute or two for the DB to fill, then refresh the window. There is no REST backfill on startup yet (see Known limitations).

**Startup banner says "WARNING: public bind without auth token"** — you launched with `--bind 0.0.0.0` (or another non-loopback address) and left `--auth-token` empty. Anyone on the network can read your data. Either bind back to `127.0.0.1` or set a token.

**`connection refused` from curl but the binary is running** — you're probably curling the wrong interface. If you started with `--bind 127.0.0.1` (default), only `http://127.0.0.1:...` works — `http://<LAN-IP>:...` will refuse. Re-launch with `--bind 0.0.0.0` (and a token!) for LAN access.

**DuckDB file keeps growing** — the 5-minute pruner deletes rows older than 24h but DuckDB doesn't reclaim disk until the file is compacted. Stop the server and run `duckdb cryexc-history.duckdb "CHECKPOINT"` to reclaim space, or just delete the file and let it rebuild on next start.

## Known limitations

- **No gap-fill on WS reconnect.** If the WebSocket drops and reconnects, trades that happened during the outage are not backfilled from Binance's REST `aggTrades` endpoint. For day-trading use cases on a reliable connection this is usually a non-issue; for long-running server deployments, expect occasional ~seconds-to-minutes gaps after reconnects.
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
