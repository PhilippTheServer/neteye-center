// Package retention runs a background job that:
//  1. Downsamples raw metrics into 1-min, 1-hour, and daily aggregates
//  2. Purges old rows past their retention window
//
// The job runs on a configurable interval (default: every 5 minutes).
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neteye/center/internal/config"
)

// Manager runs the retention and downsampling job.
type Manager struct {
	pool *pgxpool.Pool
	cfg  config.RetentionConfig
	log  *slog.Logger
}

// New creates a Manager. Call Run() in a goroutine.
func New(pool *pgxpool.Pool, cfg config.RetentionConfig, log *slog.Logger) *Manager {
	return &Manager{pool: pool, cfg: cfg, log: log}
}

// Run blocks until ctx is cancelled, running the job on each tick.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.PurgeInterval)
	defer ticker.Stop()

	// Run immediately on start.
	m.runOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.runOnce(ctx)
		}
	}
}

func (m *Manager) runOnce(ctx context.Context) {
	start := time.Now()
	if err := m.downsampleTo1Min(ctx); err != nil {
		m.log.Error("1min downsample failed", "err", err)
	}
	if err := m.downsampleTo1Hour(ctx); err != nil {
		m.log.Error("1hour downsample failed", "err", err)
	}
	if err := m.downsampleToDaily(ctx); err != nil {
		m.log.Error("daily downsample failed", "err", err)
	}
	if err := m.purgeRaw(ctx); err != nil {
		m.log.Error("purge raw failed", "err", err)
	}
	if err := m.purge1Min(ctx); err != nil {
		m.log.Error("purge 1min failed", "err", err)
	}
	if err := m.purge1Hour(ctx); err != nil {
		m.log.Error("purge 1hour failed", "err", err)
	}
	if err := m.purgeDaily(ctx); err != nil {
		m.log.Error("purge daily failed", "err", err)
	}
	m.log.Info("retention cycle done", "duration", time.Since(start).Round(time.Millisecond))
}

// downsampleTo1Min aggregates raw rows into 1-minute buckets.
// Only processes rows not yet covered by an existing 1-min aggregate.
func (m *Manager) downsampleTo1Min(ctx context.Context) error {
	_, err := m.pool.Exec(ctx, `
		INSERT INTO metrics_1min
			(time, interface_id,
			 bytes_recv_avg, bytes_sent_avg,
			 packets_recv_avg, packets_sent_avg,
			 errors_in_avg, errors_out_avg,
			 drops_in_avg, drops_out_avg,
			 sample_count)
		SELECT
			date_trunc('minute', time) AS bucket,
			interface_id,
			AVG(bytes_recv), AVG(bytes_sent),
			AVG(packets_recv), AVG(packets_sent),
			AVG(errors_in), AVG(errors_out),
			AVG(drops_in), AVG(drops_out),
			COUNT(*)
		FROM metrics_raw
		WHERE time < date_trunc('minute', NOW())  -- only complete minutes
		GROUP BY bucket, interface_id
		ON CONFLICT (interface_id, time) DO UPDATE SET
			bytes_recv_avg   = EXCLUDED.bytes_recv_avg,
			bytes_sent_avg   = EXCLUDED.bytes_sent_avg,
			packets_recv_avg = EXCLUDED.packets_recv_avg,
			packets_sent_avg = EXCLUDED.packets_sent_avg,
			errors_in_avg    = EXCLUDED.errors_in_avg,
			errors_out_avg   = EXCLUDED.errors_out_avg,
			drops_in_avg     = EXCLUDED.drops_in_avg,
			drops_out_avg    = EXCLUDED.drops_out_avg,
			sample_count     = EXCLUDED.sample_count
	`)
	return err
}

func (m *Manager) downsampleTo1Hour(ctx context.Context) error {
	_, err := m.pool.Exec(ctx, `
		INSERT INTO metrics_1hour
			(time, interface_id,
			 bytes_recv_avg, bytes_sent_avg,
			 packets_recv_avg, packets_sent_avg,
			 errors_in_avg, errors_out_avg,
			 drops_in_avg, drops_out_avg,
			 sample_count)
		SELECT
			date_trunc('hour', time) AS bucket,
			interface_id,
			AVG(bytes_recv_avg), AVG(bytes_sent_avg),
			AVG(packets_recv_avg), AVG(packets_sent_avg),
			AVG(errors_in_avg), AVG(errors_out_avg),
			AVG(drops_in_avg), AVG(drops_out_avg),
			SUM(sample_count)
		FROM metrics_1min
		WHERE time < date_trunc('hour', NOW())
		GROUP BY bucket, interface_id
		ON CONFLICT (interface_id, time) DO UPDATE SET
			bytes_recv_avg   = EXCLUDED.bytes_recv_avg,
			bytes_sent_avg   = EXCLUDED.bytes_sent_avg,
			packets_recv_avg = EXCLUDED.packets_recv_avg,
			packets_sent_avg = EXCLUDED.packets_sent_avg,
			errors_in_avg    = EXCLUDED.errors_in_avg,
			errors_out_avg   = EXCLUDED.errors_out_avg,
			drops_in_avg     = EXCLUDED.drops_in_avg,
			drops_out_avg    = EXCLUDED.drops_out_avg,
			sample_count     = EXCLUDED.sample_count
	`)
	return err
}

func (m *Manager) downsampleToDaily(ctx context.Context) error {
	_, err := m.pool.Exec(ctx, `
		INSERT INTO metrics_daily
			(date, interface_id,
			 bytes_recv_avg, bytes_sent_avg,
			 packets_recv_avg, packets_sent_avg,
			 errors_in_avg, errors_out_avg,
			 drops_in_avg, drops_out_avg,
			 sample_count)
		SELECT
			date_trunc('day', time)::date AS bucket,
			interface_id,
			AVG(bytes_recv_avg), AVG(bytes_sent_avg),
			AVG(packets_recv_avg), AVG(packets_sent_avg),
			AVG(errors_in_avg), AVG(errors_out_avg),
			AVG(drops_in_avg), AVG(drops_out_avg),
			SUM(sample_count)
		FROM metrics_1hour
		WHERE time < date_trunc('day', NOW())
		GROUP BY bucket, interface_id
		ON CONFLICT (interface_id, date) DO UPDATE SET
			bytes_recv_avg   = EXCLUDED.bytes_recv_avg,
			bytes_sent_avg   = EXCLUDED.bytes_sent_avg,
			packets_recv_avg = EXCLUDED.packets_recv_avg,
			packets_sent_avg = EXCLUDED.packets_sent_avg,
			errors_in_avg    = EXCLUDED.errors_in_avg,
			errors_out_avg   = EXCLUDED.errors_out_avg,
			drops_in_avg     = EXCLUDED.drops_in_avg,
			drops_out_avg    = EXCLUDED.drops_out_avg,
			sample_count     = EXCLUDED.sample_count
	`)
	return err
}

func (m *Manager) purgeRaw(ctx context.Context) error {
	cutoff := time.Now().Add(-m.cfg.RawKeep)
	tag, err := m.pool.Exec(ctx, `DELETE FROM metrics_raw WHERE time < $1`, cutoff)
	if err == nil && tag.RowsAffected() > 0 {
		m.log.Info("purged raw metrics", "rows", tag.RowsAffected(), "older_than", m.cfg.RawKeep)
	}
	return err
}

func (m *Manager) purge1Min(ctx context.Context) error {
	cutoff := time.Now().Add(-m.cfg.MinKeep)
	_, err := m.pool.Exec(ctx, `DELETE FROM metrics_1min WHERE time < $1`, cutoff)
	return err
}

func (m *Manager) purge1Hour(ctx context.Context) error {
	cutoff := time.Now().Add(-m.cfg.HourKeep)
	_, err := m.pool.Exec(ctx, `DELETE FROM metrics_1hour WHERE time < $1`, cutoff)
	return err
}

func (m *Manager) purgeDaily(ctx context.Context) error {
	if m.cfg.DailyKeep == 0 {
		return nil // keep forever
	}
	cutoff := time.Now().Add(-m.cfg.DailyKeep)
	_, err := m.pool.Exec(ctx, `DELETE FROM metrics_daily WHERE date < $1`, cutoff.Format("2006-01-02"))
	return err
}
