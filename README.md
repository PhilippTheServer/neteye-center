# neteye-center

The central aggregation and API server for the neteye network monitoring system.
It receives real-time telemetry from agents over WebSocket, stores historical data
in PostgreSQL, maintains an in-memory topology graph, and serves a live WebSocket
stream plus REST API to the frontend.

## Overview

```
agents (many)  ──WS──▶  :9090  ┐
                                ├── neteye-center ──▶  PostgreSQL
frontend       ──WS──▶  :8080  ┘
               ──HTTP▶  :8080
```

Two listeners run concurrently:

| Port | Purpose |
|------|---------|
| `9090` | Agent WebSocket endpoint (`/ws`) |
| `8080` | Frontend WebSocket (`/ws`) + REST API (`/api/…`) |

## Requirements

- Go 1.24+
- PostgreSQL 14+

## Running

### Directly

```bash
# Minimal — uses default config (expects postgres on localhost:5432)
go run ./cmd/center

# With a config file
go run ./cmd/center -config config.yaml

# Override via environment variables
NETEYE_DB_DSN="postgres://user:pass@host:5432/neteye?sslmode=disable" \
NETEYE_AGENT_ADDR=":9090" \
NETEYE_FRONTEND_ADDR=":8080" \
  go run ./cmd/center
```

### Docker

```bash
docker build -t neteye-center .
docker run -e NETEYE_DB_DSN="postgres://..." -p 8080:8080 -p 9090:9090 neteye-center
```

### docker-compose

See the root `docker-compose.yml` — the center starts automatically after postgres is healthy.

## Configuration

All fields can be set in a YAML file (`-config path/to/config.yaml`) **and/or** via
environment variables. Environment variables take precedence.

```yaml
server:
  agent_addr:      ":9090"       # WebSocket address for agents
  frontend_addr:   ":8080"       # HTTP/WebSocket address for the frontend
  offline_timeout: "30s"         # device marked offline after this many seconds without a heartbeat

database:
  dsn:       "postgres://neteye:neteye@localhost:5432/neteye?sslmode=disable"
  max_conns: 20                  # pgx connection pool size

retention:
  raw_interval:   "5s"           # cadence agents send updates (used for rate display only)
  raw_keep:       "1h"           # how long raw 5 s rows are kept
  min_keep:       "24h"          # how long 1-minute aggregates are kept
  hour_keep:      "720h"         # how long 1-hour aggregates are kept  (30 days)
  daily_keep:     "8760h"        # how long daily aggregates are kept   (1 year, 0 = forever)
  purge_interval: "5m"           # how often the retention job runs
```

| Environment variable   | Overrides                 |
|------------------------|---------------------------|
| `NETEYE_DB_DSN`        | `database.dsn`            |
| `NETEYE_AGENT_ADDR`    | `server.agent_addr`       |
| `NETEYE_FRONTEND_ADDR` | `server.frontend_addr`    |

## API Reference

### WebSocket — `/ws` (frontend)

Connect with any WebSocket client. On connect you receive a full topology snapshot;
afterwards the server pushes incremental updates as they arrive from agents.

**Message types received:**

| `type` | Payload field | Description |
|--------|--------------|-------------|
| `topology` | `topology` | Full snapshot of all known devices (sent once on connect) |
| `device_update` | `device_update` | One device's interfaces, addresses, and routes changed |
| `device_offline` | `device_offline` | A device stopped reporting |
| `metrics` | `metrics` | Per-interface rate metrics (bytes/s, packets/s, errors/s) |

### REST

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/topology` | Current topology snapshot (same as the initial WS push) |
| `GET` | `/api/devices` | All devices with full interface and route data (from DB) |
| `GET` | `/api/route?src=<id>&dst=<id>` | BFS route analysis between two device IDs |
| `GET` | `/api/metrics?iface=<id>&from=<RFC3339>&to=<RFC3339>` | Raw metric rows for a time window |
| `GET` | `/api/stats` | Live counts: total/online devices, connected frontend clients |
| `GET` | `/health` | Liveness probe |

### WebSocket — `/ws` (agents, port 9090)

Agents connect here. The first message must be a `register` frame; subsequent
messages are `update` frames. See the [PROJECT.md](PROJECT.md) wire protocol section
for the full schema.

## Database Schema

Tables are created automatically on first start via the built-in migration runner.

| Table | Retention | Notes |
|-------|-----------|-------|
| `devices` | permanent | One row per hostname |
| `interfaces` | permanent | One row per interface per device |
| `interface_addresses` | permanent | One row per IP/prefix per interface |
| `routes` | permanent | Replaced wholesale on every agent update |
| `metrics_raw` | 1 h | Raw counters at agent interval (~5 s) |
| `metrics_1min` | 24 h | 1-minute averages, computed by retention job |
| `metrics_1hour` | 30 d | 1-hour averages |
| `metrics_daily` | 1 y | Daily averages |

## Project structure

```
neteye-center/
├── cmd/center/main.go          # Entry point, wires all components
├── internal/
│   ├── config/config.go        # YAML + env-var config loading
│   ├── db/
│   │   ├── postgres.go         # Pool setup
│   │   ├── migrations.go       # Auto-applying schema migrations
│   │   └── queries.go          # All SQL queries
│   ├── models/models.go        # Shared wire protocol types
│   ├── hub/
│   │   ├── agent_hub.go        # Agent WebSocket handler + rate computation
│   │   └── frontend_hub.go     # Frontend broadcast hub
│   ├── topology/store.go       # In-memory graph + BFS route analysis
│   ├── api/router.go           # HTTP handler registration
│   └── retention/manager.go   # Downsampling + purge job
├── Dockerfile
└── go.mod
```
