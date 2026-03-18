# NetEye — Network Topology Monitor

NetEye is a real-time network monitoring system designed for Linux environments
from single-server setups to large HPC clusters with thousands of nodes.

It visualises every device as a node in a live, interactive topology map.
Devices sharing a subnet are automatically connected by a labelled edge.
Live traffic metrics flow continuously from each node's kernel counters, and
a built-in path tracer shows exactly how traffic is routed between any two
devices.

---

## Table of contents

1. [Architecture](#architecture)
2. [Components](#components)
3. [Data flow](#data-flow)
4. [Wire protocol](#wire-protocol)
5. [Database schema](#database-schema)
6. [Data retention](#data-retention)
7. [Route analysis](#route-analysis)
8. [Deployment](#deployment)
9. [Configuration reference](#configuration-reference)
10. [Extending neteye](#extending-neteye)

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│  Monitored nodes (Linux)                                        │
│                                                                 │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐         │
│  │ neteye-agent │  │ neteye-agent │  │ neteye-agent │  ...    │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘         │
│         │ WebSocket        │                  │                  │
└─────────┼──────────────────┼──────────────────┼─────────────────┘
          │  :9090           │                  │
          ▼                  ▼                  ▼
┌─────────────────────────────────────────────────────────────────┐
│  neteye-center                                                  │
│                                                                 │
│  ┌─────────────┐    ┌───────────────┐    ┌──────────────────┐  │
│  │  Agent Hub  │───▶│ Topology Store│───▶│  Frontend Hub    │  │
│  │  (port 9090)│    │  (in-memory)  │    │  (port 8080 /ws) │  │
│  └──────┬──────┘    └───────────────┘    └──────────────────┘  │
│         │                                         ▲             │
│         ▼                                         │             │
│  ┌─────────────┐                        ┌─────────────────┐    │
│  │  PostgreSQL │◀── retention job ──────│  REST API       │    │
│  │  (metrics,  │                        │  (port 8080)    │    │
│  │   topology) │                        └────────┬────────┘    │
│  └─────────────┘                                 │             │
└──────────────────────────────────────────────────┼─────────────┘
                                                   │ HTTP + WS
                                          ┌────────▼────────┐
                                          │ neteye-frontend │
                                          │ (nginx / ng     │
                                          │  serve)         │
                                          └─────────────────┘
                                                   │
                                             Browser
```

### Key design choices

| Decision | Rationale |
|----------|-----------|
| Go for center and agent | Single static binary, cross-compile to any Linux arch, low memory footprint |
| WebSocket (not gRPC / MQTT) | Low-overhead persistent connections; works through most firewalls and proxies |
| In-memory topology store | Sub-millisecond reads for every frontend push; PostgreSQL is only the write-ahead store |
| Angular Signals (not RxJS) | Simpler reactive graph; D3 runs outside Angular's zone to avoid change-detection overhead |
| BFS route analysis on the server | Server has the full routing table; client BFS is only used for instant UI feedback |
| PostgreSQL (not InfluxDB / TimescaleDB) | Fewer dependencies; the tiered aggregation pattern reproduces time-series semantics with standard SQL |

---

## Components

### neteye-agent

Deployed on every monitored node. Reads network state from the Linux kernel
and pushes it to the center over a persistent WebSocket.

**Collectors (all Linux-specific):**

| Collector | Kernel source | Data |
|-----------|--------------|------|
| `interfaces.go` | netlink `LinkList` + `AddrList` | Interface name, MAC, state, MTU, IPv4/IPv6 addresses |
| `metrics.go` | `/proc/net/dev` | Monotonic byte/packet/error/drop counters |
| `routes.go` | netlink `RouteList` (table 254 / main) | Destination CIDR, gateway, interface, metric, flags |

The agent sends a `register` message on connect (hostname, OS, arch, version),
then a full `update` message every `collect_interval` (default 5 s).
On disconnect it reconnects with exponential back-off (2 s → 30 s).

### neteye-center

The central hub. Runs two HTTP servers in parallel.

**Agent server (`:9090`):**
- Upgrades each incoming TCP connection to WebSocket
- Validates the `register` message; upserts the device in PostgreSQL
- Spawns a goroutine per connection — scales to thousands of concurrent agents
- Computes per-interface rates (bytes/s, packets/s, …) from successive counter samples
- Marks devices offline when their WebSocket disconnects

**Frontend server (`:8080`):**
- `/ws` — WebSocket stream; new clients receive a full topology snapshot instantly
- `/api/*` — REST endpoints (see neteye-center README)

**Topology store:**
A `sync.RWMutex`-protected `map[string]*DeviceInfo` that mirrors the live state
of all connected devices. All frontend pushes are served from this map — no
database round-trip on the hot path.

**Retention manager:**
A background goroutine that wakes every `purge_interval` (default 5 min) and:
1. Aggregates `metrics_raw` → `metrics_1min` (upserts, idempotent)
2. Aggregates `metrics_1min` → `metrics_1hour`
3. Aggregates `metrics_1hour` → `metrics_daily`
4. Deletes rows older than the configured keep windows

### neteye-frontend

Angular 21 single-page application.

**State management:**
`TopologyService` holds the entire application state as Angular Signals:
- `devices: Signal<Map<string, DeviceInfo>>` — the live device registry
- `connected: Signal<boolean>` — WebSocket connection status
- `graphNodes` / `graphLinks` — computed from devices; drive the D3 simulation
- Per-interface metrics ring buffers (last 300 samples ≈ 25 min at 5 s) — read
  by the metrics chart without signals (rAF loop polls directly)

**D3 graph:**
Force simulation with `forceManyBody`, `forceLink`, `forceCenter`, and
`forceCollide`. The simulation is restarted at `alpha(0.3)` on every topology
change, so new nodes settle naturally without a full restart. All SVG mutations
happen outside Angular's NgZone for performance.

---

## Data flow

### Agent → center → frontend (normal update)

```
1. Agent collects kernel state (5 s interval)
2. Agent sends AgentMessage{type:"update", update:{interfaces, routes}} over WS
3. AgentHub.processUpdate():
   a. UpsertInterfaces() — writes interface + address rows to PostgreSQL
   b. ReplaceRoutes()    — atomically replaces route rows for this device
   c. InsertRawMetrics() — writes one metrics_raw row per interface
   d. computeRate()      — subtracts previous counters, divides by dt → rates
   e. topo.Upsert()      — updates in-memory topology store
   f. frontendHub.BroadcastJSON(device_update) — pushes to all WS clients
   g. frontendHub.BroadcastJSON(metrics) per interface with non-zero rates
4. Frontend TopologyService.handleMessage():
   a. Updates devices signal
   b. Pushes metrics to ring buffer
5. Angular computed signals recalculate graphNodes + graphLinks
6. D3 effect fires, calls updateGraph(), simulation restarts at alpha(0.3)
7. Metrics chart rAF loop reads ring buffer and redraws the SVG
```

### Frontend connects (initial load)

```
1. Browser opens WebSocket to /ws
2. FrontendHub sends topology snapshot (all devices from in-memory store)
3. Frontend populates devices signal → graph renders immediately
4. Subsequent updates arrive incrementally
```

### Device goes offline

```
1. Agent process exits / network lost → WebSocket closes
2. AgentHub deferred cleanup:
   a. db.MarkDeviceOffline() — sets status='offline' in PostgreSQL
   b. topo.SetOffline()      — updates in-memory store
   c. frontendHub.BroadcastJSON(device_offline)
3. Frontend marks device offline (greyed-out status dot on node)
```

---

## Wire protocol

All messages are JSON over WebSocket (text frames).

### Agent → Center

#### `register` (sent once, immediately on connect)

```json
{
  "type": "register",
  "register": {
    "hostname":      "compute-node-042",
    "os":            "linux",
    "arch":          "amd64",
    "agent_version": "1.0.0"
  }
}
```

#### `update` (sent every `collect_interval`)

```json
{
  "type": "update",
  "update": {
    "timestamp": "2026-03-18T14:00:05Z",
    "interfaces": [
      {
        "name":       "eth0",
        "mac":        "aa:bb:cc:dd:ee:ff",
        "state":      "up",
        "mtu":        1500,
        "speed_mbps": 10000,
        "addresses": [
          { "address": "192.168.1.42/24", "family": "ipv4" },
          { "address": "2001:db8::1/64",  "family": "ipv6" }
        ],
        "metrics": {
          "bytes_recv":   1234567890,
          "bytes_sent":   987654321,
          "packets_recv": 1000000,
          "packets_sent": 800000,
          "errors_in":    0,
          "errors_out":   0,
          "drops_in":     0,
          "drops_out":    0
        }
      }
    ],
    "routes": [
      {
        "destination":    "0.0.0.0/0",
        "gateway":        "192.168.1.1",
        "interface_name": "eth0",
        "metric":         100,
        "flags":          "UG"
      }
    ]
  }
}
```

> Metric values are **absolute monotonic counters** from the kernel.
> neteye-center computes rates by comparing successive samples.

### Center → Frontend

#### `topology` (sent once on WebSocket connect)

```json
{
  "type": "topology",
  "topology": {
    "timestamp": "2026-03-18T14:00:00Z",
    "devices": [ /* array of DeviceInfo — see device_update below */ ]
  }
}
```

#### `device_update`

```json
{
  "type": "device_update",
  "device_update": {
    "id":         "550e8400-e29b-41d4-a716-446655440000",
    "hostname":   "compute-node-042",
    "os":         "linux",
    "arch":       "amd64",
    "status":     "online",
    "first_seen": "2026-03-18T09:00:00Z",
    "last_seen":  "2026-03-18T14:00:05Z",
    "interfaces": [ /* same structure as agent update */ ],
    "routes":     [ /* same structure as agent update */ ]
  }
}
```

#### `device_offline`

```json
{
  "type": "device_offline",
  "device_offline": {
    "device_id": "550e8400-e29b-41d4-a716-446655440000",
    "hostname":  "compute-node-042",
    "last_seen": "2026-03-18T14:00:05Z"
  }
}
```

#### `metrics`

```json
{
  "type": "metrics",
  "metrics": {
    "device_id":       "550e8400-e29b-41d4-a716-446655440000",
    "interface_name":  "eth0",
    "timestamp":       "2026-03-18T14:00:05Z",
    "bytes_recv_rate":   125000000.0,
    "bytes_sent_rate":   80000000.0,
    "packets_recv_rate": 90000.0,
    "packets_sent_rate": 70000.0,
    "errors_in_rate":    0.0,
    "errors_out_rate":   0.0,
    "drops_in_rate":     0.0,
    "drops_out_rate":    0.0
  }
}
```

---

## Database schema

```
devices
  id           UUID  PK
  hostname     TEXT  UNIQUE
  os           TEXT
  arch         TEXT
  agent_version TEXT
  status       TEXT  ('online' | 'offline')
  first_seen   TIMESTAMPTZ
  last_seen    TIMESTAMPTZ

interfaces
  id           UUID  PK
  device_id    UUID  FK → devices
  name         TEXT
  mac          TEXT
  state        TEXT
  mtu          INT
  speed_mbps   BIGINT
  UNIQUE (device_id, name)

interface_addresses
  id           UUID  PK
  interface_id UUID  FK → interfaces
  address      CIDR
  family       TEXT  ('ipv4' | 'ipv6')
  UNIQUE (interface_id, address)

routes
  id             UUID  PK
  device_id      UUID  FK → devices
  destination    CIDR  (nullable = default route)
  gateway        INET  (nullable = on-link)
  interface_name TEXT
  metric         INT
  flags          TEXT
  updated_at     TIMESTAMPTZ

metrics_raw        — 5 s resolution, retained 1 h
metrics_1min       — 1 min averages, retained 24 h
metrics_1hour      — 1 hour averages, retained 30 d
metrics_daily      — daily averages, retained 1 y

All metrics tables share the same columns:
  time / date      TIMESTAMPTZ / DATE  (PK component)
  interface_id     UUID  FK → interfaces  (PK component)
  bytes_recv_avg   DOUBLE PRECISION
  bytes_sent_avg   DOUBLE PRECISION
  packets_recv_avg DOUBLE PRECISION
  packets_sent_avg DOUBLE PRECISION
  errors_in_avg    DOUBLE PRECISION
  errors_out_avg   DOUBLE PRECISION
  drops_in_avg     DOUBLE PRECISION
  drops_out_avg    DOUBLE PRECISION
  sample_count     INT
```

Schema migrations are applied automatically at startup via the built-in
migration runner (`internal/db/migrations.go`). Each migration is idempotent
(`IF NOT EXISTS`, `ON CONFLICT DO NOTHING`).

---

## Data retention

The retention manager runs every `purge_interval` (default 5 min).

```
metrics_raw  ──(aggregate)──▶  metrics_1min  ──(aggregate)──▶  metrics_1hour  ──(aggregate)──▶  metrics_daily
    │                               │                                │                                │
    └── purge after 1 h             └── purge after 24 h            └── purge after 30 d             └── purge after 1 y
```

Aggregation uses `date_trunc` on complete time buckets only (`< date_trunc('minute', NOW())`),
so in-flight data is never truncated. All aggregation queries use `ON CONFLICT DO UPDATE`,
making them safe to run repeatedly.

To tune retention windows, edit `retention:` in the config file or set the
relevant environment variables (see [configuration reference](#configuration-reference)).

---

## Route analysis

The topology graph is built from shared subnets: two devices are adjacent if
they have at least one IPv4 address on the same network prefix. This models
direct Layer-3 reachability without requiring an explicit neighbour discovery
protocol.

`topology.Store.AnalyzeRoute(srcID, dstID)` runs a breadth-first search over
this adjacency graph:

1. Build a `cidr → [{deviceID, ip}]` index from all current interface addresses.
2. For each CIDR with ≥ 2 members, add edges between all member pairs.
3. BFS from `srcID` to `dstID`, tracking the CIDR and IPs used at each hop.
4. Return the first path found (shortest by hop count).

The result includes:
- Ordered list of hops with device ID, hostname, the shared CIDR, and both
  endpoint IPs at each hop
- List of unique networks traversed

This approach correctly handles:
- Multi-homed devices (connected to multiple networks simultaneously)
- VPN tunnels (appear as regular subnets)
- Asymmetric topologies (e.g. router with LAN + DMZ + management interfaces)

---

## Deployment

### Local development

```bash
# 1. Start PostgreSQL and the center
docker compose up postgres neteye-center

# 2. Start the frontend dev server (hot reload)
cd neteye-frontend && ng serve

# 3. Run an agent on this machine (requires Linux)
cd neteye-agent && go run ./cmd/agent
# Open http://localhost:4200
```

### Full docker-compose stack

```bash
docker compose up --build
# Frontend: http://localhost
# Center API: http://localhost:8080
# Agent WebSocket: ws://localhost:9090/ws
```

The agent block in `docker-compose.yml` is commented out. The agent must run
with `--network host` to read real interface data — in Compose this prevents it
from joining the internal network. For cluster deployments use systemd or a
Kubernetes DaemonSet (see neteye-agent README).

### Production checklist

- [ ] Set a strong PostgreSQL password and remove the exposed `5432` port from
      `docker-compose.yml`
- [ ] Run the center behind a TLS-terminating reverse proxy (nginx / Caddy /
      Traefik); the center itself does not handle TLS
- [ ] Set `NETEYE_CENTER` to the WSS address on all agents
      (`wss://monitor.your-domain.com/ws-agent`)
- [ ] Tune `retention.*` to match your storage budget
- [ ] Set `server.offline_timeout` based on your expected agent update interval
      (rule of thumb: 6× the `collect_interval`)
- [ ] For large clusters (> 500 nodes) increase `database.max_conns` and ensure
      PostgreSQL `max_connections` is set accordingly

---

## Configuration reference

### neteye-center

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `server.agent_addr` | `NETEYE_AGENT_ADDR` | `:9090` | Agent WebSocket listen address |
| `server.frontend_addr` | `NETEYE_FRONTEND_ADDR` | `:8080` | Frontend HTTP/WS listen address |
| `server.offline_timeout` | — | `30s` | Inactivity before device is marked offline |
| `database.dsn` | `NETEYE_DB_DSN` | `postgres://neteye:neteye@localhost:5432/neteye?sslmode=disable` | PostgreSQL DSN |
| `database.max_conns` | — | `20` | Connection pool size |
| `retention.raw_keep` | — | `1h` | Retention for raw metrics |
| `retention.min_keep` | — | `24h` | Retention for 1-minute aggregates |
| `retention.hour_keep` | — | `720h` | Retention for 1-hour aggregates |
| `retention.daily_keep` | — | `8760h` | Retention for daily aggregates (0 = forever) |
| `retention.purge_interval` | — | `5m` | How often the retention job runs |

### neteye-agent

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `center_url` | `NETEYE_CENTER` | `ws://localhost:9090/ws` | Center agent WebSocket URL |
| `hostname` | `NETEYE_HOSTNAME` | `os.Hostname()` | Override reported hostname |
| `collect_interval` | — | `5s` | How often to sample and push |
| `reconnect_delay` | — | `5s` | Initial reconnect delay (backs off to 30 s) |

---

## Extending neteye

### Adding a new metric

1. Add the counter field to `InterfaceMetrics` in `neteye-center/internal/models/models.go`
   and mirror it in the `ifaceMetrics` struct in `neteye-agent/internal/client/client.go`.
2. Add the column to `metrics_raw` (and the aggregation tables) in
   `neteye-center/internal/db/migrations.go` — add a new migration entry.
3. Read the value in `neteye-agent/internal/collector/metrics.go`.
4. Update `InsertRawMetrics` and `MetricsUpdate` computation in
   `neteye-center/internal/hub/agent_hub.go` and `db/queries.go`.
5. Add the series key to `MetricsChartComponent.getSeriesKeys()` in the frontend.

### Adding a new collector (e.g. CPU load)

Create a new file in `neteye-agent/internal/collector/` with a `//go:build linux`
guard. Add the collected data to `AgentUpdate` in the models, update the agent's
`collectUpdate()` call in `client.go`, and add the corresponding storage and
display logic in the center and frontend.

### Running multiple centers (horizontal scale)

The center is currently stateful (in-memory topology store). For multi-center
deployments:
- Use a shared PostgreSQL instance (already supported)
- Publish topology changes via PostgreSQL `LISTEN/NOTIFY` or an external message
  bus (e.g. Redis Pub/Sub) so all center instances broadcast the same events to
  their connected frontend clients
- Put a WebSocket-aware load balancer (e.g. nginx with `ip_hash`) in front so
  each browser client sticks to one center instance
