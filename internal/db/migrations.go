// Package db handles PostgreSQL connectivity, schema migrations, and all data-access queries.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrations are applied in order. Each entry is idempotent (IF NOT EXISTS / OR REPLACE).
var migrations = []string{
	// ── Schema version table ──────────────────────────────────────────────
	`CREATE TABLE IF NOT EXISTS schema_migrations (
		version   INT  PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,

	// ── Devices ───────────────────────────────────────────────────────────
	`CREATE TABLE IF NOT EXISTS devices (
		id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
		hostname     TEXT        UNIQUE NOT NULL,
		os           TEXT        NOT NULL DEFAULT '',
		arch         TEXT        NOT NULL DEFAULT '',
		agent_version TEXT       NOT NULL DEFAULT '',
		status       TEXT        NOT NULL DEFAULT 'online',
		first_seen   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_seen    TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,

	// ── Network interfaces (structural, not metric) ────────────────────────
	`CREATE TABLE IF NOT EXISTS interfaces (
		id         UUID  PRIMARY KEY DEFAULT gen_random_uuid(),
		device_id  UUID  NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
		name       TEXT  NOT NULL,
		mac        TEXT  NOT NULL DEFAULT '',
		state      TEXT  NOT NULL DEFAULT 'down',
		mtu        INT   NOT NULL DEFAULT 0,
		speed_mbps BIGINT NOT NULL DEFAULT 0,
		UNIQUE (device_id, name)
	)`,

	// ── Interface addresses (one row per IP/prefix) ───────────────────────
	`CREATE TABLE IF NOT EXISTS interface_addresses (
		id           UUID  PRIMARY KEY DEFAULT gen_random_uuid(),
		interface_id UUID  NOT NULL REFERENCES interfaces(id) ON DELETE CASCADE,
		address      INET  NOT NULL,
		family       TEXT  NOT NULL,
		UNIQUE (interface_id, address)
	)`,

	// ── Routing table (replaced wholesale on each agent update) ───────────
	`CREATE TABLE IF NOT EXISTS routes (
		id             UUID  PRIMARY KEY DEFAULT gen_random_uuid(),
		device_id      UUID  NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
		destination    CIDR,
		gateway        INET,
		interface_name TEXT  NOT NULL DEFAULT '',
		metric         INT   NOT NULL DEFAULT 0,
		flags          TEXT  NOT NULL DEFAULT '',
		updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE INDEX IF NOT EXISTS routes_device_idx ON routes(device_id)`,

	// ── Raw metrics (5 s resolution, retained 1 h) ────────────────────────
	`CREATE TABLE IF NOT EXISTS metrics_raw (
		time          TIMESTAMPTZ NOT NULL,
		interface_id  UUID        NOT NULL REFERENCES interfaces(id) ON DELETE CASCADE,
		bytes_recv    BIGINT      NOT NULL DEFAULT 0,
		bytes_sent    BIGINT      NOT NULL DEFAULT 0,
		packets_recv  BIGINT      NOT NULL DEFAULT 0,
		packets_sent  BIGINT      NOT NULL DEFAULT 0,
		errors_in     BIGINT      NOT NULL DEFAULT 0,
		errors_out    BIGINT      NOT NULL DEFAULT 0,
		drops_in      BIGINT      NOT NULL DEFAULT 0,
		drops_out     BIGINT      NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS metrics_raw_time_idx ON metrics_raw(time DESC)`,
	`CREATE INDEX IF NOT EXISTS metrics_raw_iface_time_idx ON metrics_raw(interface_id, time DESC)`,

	// ── 1-minute aggregates (retained 24 h) ──────────────────────────────
	`CREATE TABLE IF NOT EXISTS metrics_1min (
		time             TIMESTAMPTZ NOT NULL,
		interface_id     UUID        NOT NULL REFERENCES interfaces(id) ON DELETE CASCADE,
		bytes_recv_avg   DOUBLE PRECISION NOT NULL DEFAULT 0,
		bytes_sent_avg   DOUBLE PRECISION NOT NULL DEFAULT 0,
		packets_recv_avg DOUBLE PRECISION NOT NULL DEFAULT 0,
		packets_sent_avg DOUBLE PRECISION NOT NULL DEFAULT 0,
		errors_in_avg    DOUBLE PRECISION NOT NULL DEFAULT 0,
		errors_out_avg   DOUBLE PRECISION NOT NULL DEFAULT 0,
		drops_in_avg     DOUBLE PRECISION NOT NULL DEFAULT 0,
		drops_out_avg    DOUBLE PRECISION NOT NULL DEFAULT 0,
		sample_count     INT         NOT NULL DEFAULT 0,
		PRIMARY KEY (interface_id, time)
	)`,
	`CREATE INDEX IF NOT EXISTS metrics_1min_time_idx ON metrics_1min(time DESC)`,

	// ── 1-hour aggregates (retained 30 d) ────────────────────────────────
	`CREATE TABLE IF NOT EXISTS metrics_1hour (
		time             TIMESTAMPTZ NOT NULL,
		interface_id     UUID        NOT NULL REFERENCES interfaces(id) ON DELETE CASCADE,
		bytes_recv_avg   DOUBLE PRECISION NOT NULL DEFAULT 0,
		bytes_sent_avg   DOUBLE PRECISION NOT NULL DEFAULT 0,
		packets_recv_avg DOUBLE PRECISION NOT NULL DEFAULT 0,
		packets_sent_avg DOUBLE PRECISION NOT NULL DEFAULT 0,
		errors_in_avg    DOUBLE PRECISION NOT NULL DEFAULT 0,
		errors_out_avg   DOUBLE PRECISION NOT NULL DEFAULT 0,
		drops_in_avg     DOUBLE PRECISION NOT NULL DEFAULT 0,
		drops_out_avg    DOUBLE PRECISION NOT NULL DEFAULT 0,
		sample_count     INT         NOT NULL DEFAULT 0,
		PRIMARY KEY (interface_id, time)
	)`,

	// ── Daily aggregates (retained 1 y / configurable) ───────────────────
	`CREATE TABLE IF NOT EXISTS metrics_daily (
		date             DATE        NOT NULL,
		interface_id     UUID        NOT NULL REFERENCES interfaces(id) ON DELETE CASCADE,
		bytes_recv_avg   DOUBLE PRECISION NOT NULL DEFAULT 0,
		bytes_sent_avg   DOUBLE PRECISION NOT NULL DEFAULT 0,
		packets_recv_avg DOUBLE PRECISION NOT NULL DEFAULT 0,
		packets_sent_avg DOUBLE PRECISION NOT NULL DEFAULT 0,
		errors_in_avg    DOUBLE PRECISION NOT NULL DEFAULT 0,
		errors_out_avg   DOUBLE PRECISION NOT NULL DEFAULT 0,
		drops_in_avg     DOUBLE PRECISION NOT NULL DEFAULT 0,
		drops_out_avg    DOUBLE PRECISION NOT NULL DEFAULT 0,
		sample_count     INT         NOT NULL DEFAULT 0,
		PRIMARY KEY (interface_id, date)
	)`,

	// ── Fix interface_addresses.address: CIDR → INET so host-bit addresses
	// like 2a01:4f8:c17:d7b2::1/64 are accepted (CIDR requires zero host bits).
	`ALTER TABLE interface_addresses
		ALTER COLUMN address TYPE INET USING address::inet`,
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	// Ensure schema_migrations exists first
	if _, err := pool.Exec(ctx, migrations[0]); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	// Find highest applied version
	var applied int
	_ = pool.QueryRow(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&applied)

	for i, sql := range migrations[1:] {
		version := i + 1
		if version <= applied {
			continue
		}
		if _, err := pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, version); err != nil {
			return fmt.Errorf("record migration %d: %w", version, err)
		}
	}
	return nil
}
