package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neteye/center/internal/models"
)

// UpsertDevice inserts or updates a device record, returning its UUID.
func UpsertDevice(ctx context.Context, pool *pgxpool.Pool, reg *models.AgentRegistration) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO devices (hostname, os, arch, agent_version, status, last_seen)
		VALUES ($1, $2, $3, $4, 'online', NOW())
		ON CONFLICT (hostname) DO UPDATE SET
			os            = EXCLUDED.os,
			arch          = EXCLUDED.arch,
			agent_version = EXCLUDED.agent_version,
			status        = 'online',
			last_seen     = NOW()
		RETURNING id`,
		reg.Hostname, reg.OS, reg.Arch, reg.AgentVersion,
	).Scan(&id)
	return id, err
}

// MarkDeviceOffline sets a device's status to offline and records last_seen.
func MarkDeviceOffline(ctx context.Context, pool *pgxpool.Pool, deviceID string) error {
	_, err := pool.Exec(ctx, `
		UPDATE devices SET status='offline', last_seen=NOW() WHERE id=$1`, deviceID)
	return err
}

// UpsertInterfaces replaces the interface + address records for a device.
// Returns a map from interface name → UUID.
func UpsertInterfaces(ctx context.Context, pool *pgxpool.Pool, deviceID string, ifaces []models.InterfaceInfo) (map[string]string, error) {
	ids := make(map[string]string, len(ifaces))

	for _, iface := range ifaces {
		var id string
		err := pool.QueryRow(ctx, `
			INSERT INTO interfaces (device_id, name, mac, state, mtu, speed_mbps)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (device_id, name) DO UPDATE SET
				mac        = EXCLUDED.mac,
				state      = EXCLUDED.state,
				mtu        = EXCLUDED.mtu,
				speed_mbps = EXCLUDED.speed_mbps
			RETURNING id`,
			deviceID, iface.Name, iface.MAC, iface.State, iface.MTU, iface.SpeedMbps,
		).Scan(&id)
		if err != nil {
			return nil, err
		}
		ids[iface.Name] = id

		// Replace addresses for this interface
		if _, err := pool.Exec(ctx,
			`DELETE FROM interface_addresses WHERE interface_id=$1`, id); err != nil {
			return nil, err
		}
		for _, addr := range iface.Addresses {
			if _, err := pool.Exec(ctx, `
				INSERT INTO interface_addresses (interface_id, address, family)
				VALUES ($1, $2::inet, $3)
				ON CONFLICT DO NOTHING`,
				id, addr.Address, addr.Family,
			); err != nil {
				return nil, err
			}
		}
	}
	return ids, nil
}

// ReplaceRoutes deletes all existing routes for a device and inserts fresh ones.
func ReplaceRoutes(ctx context.Context, pool *pgxpool.Pool, deviceID string, routes []models.Route) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `DELETE FROM routes WHERE device_id=$1`, deviceID); err != nil {
		return err
	}
	for _, r := range routes {
		dst := nilIfEmpty(r.Destination)
		gw := nilIfEmpty(r.Gateway)
		if _, err := tx.Exec(ctx, `
			INSERT INTO routes (device_id, destination, gateway, interface_name, metric, flags, updated_at)
			VALUES ($1, $2::cidr, $3::inet, $4, $5, $6, NOW())`,
			deviceID, dst, gw, r.InterfaceName, r.Metric, r.Flags,
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// InsertRawMetrics writes a batch of raw metric rows.
func InsertRawMetrics(ctx context.Context, pool *pgxpool.Pool, t time.Time, ifaceID string, m models.InterfaceMetrics) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO metrics_raw
			(time, interface_id, bytes_recv, bytes_sent, packets_recv, packets_sent,
			 errors_in, errors_out, drops_in, drops_out)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		t, ifaceID,
		m.BytesRecv, m.BytesSent, m.PacketsRecv, m.PacketsSent,
		m.ErrorsIn, m.ErrorsOut, m.DropsIn, m.DropsOut,
	)
	return err
}

// LoadAllDevices returns every device with its interfaces and addresses.
func LoadAllDevices(ctx context.Context, pool *pgxpool.Pool) ([]models.DeviceInfo, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, hostname, os, arch, status, first_seen, last_seen
		FROM devices ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []models.DeviceInfo
	for rows.Next() {
		var d models.DeviceInfo
		if err := rows.Scan(&d.ID, &d.Hostname, &d.OS, &d.Arch, &d.Status, &d.FirstSeen, &d.LastSeen); err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load interfaces + addresses for each device
	for i := range devices {
		ifaces, err := loadInterfaces(ctx, pool, devices[i].ID)
		if err != nil {
			return nil, err
		}
		devices[i].Interfaces = ifaces

		routes, err := loadRoutes(ctx, pool, devices[i].ID)
		if err != nil {
			return nil, err
		}
		devices[i].Routes = routes
	}
	return devices, nil
}

func loadInterfaces(ctx context.Context, pool *pgxpool.Pool, deviceID string) ([]models.InterfaceInfo, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, name, mac, state, mtu, speed_mbps FROM interfaces WHERE device_id=$1 ORDER BY name`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ifaces []models.InterfaceInfo
	var ids []string
	for rows.Next() {
		var id string
		var iface models.InterfaceInfo
		if err := rows.Scan(&id, &iface.Name, &iface.MAC, &iface.State, &iface.MTU, &iface.SpeedMbps); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		ifaces = append(ifaces, iface)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i, id := range ids {
		addrs, err := loadAddresses(ctx, pool, id)
		if err != nil {
			return nil, err
		}
		ifaces[i].Addresses = addrs
	}
	return ifaces, nil
}

func loadAddresses(ctx context.Context, pool *pgxpool.Pool, ifaceID string) ([]models.AddressInfo, error) {
	rows, err := pool.Query(ctx, `
		SELECT address::text, family FROM interface_addresses WHERE interface_id=$1`, ifaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var addrs []models.AddressInfo
	for rows.Next() {
		var a models.AddressInfo
		if err := rows.Scan(&a.Address, &a.Family); err != nil {
			return nil, err
		}
		addrs = append(addrs, a)
	}
	return addrs, rows.Err()
}

func loadRoutes(ctx context.Context, pool *pgxpool.Pool, deviceID string) ([]models.Route, error) {
	rows, err := pool.Query(ctx, `
		SELECT COALESCE(destination::text,''), COALESCE(gateway::text,''), interface_name, metric, flags
		FROM routes WHERE device_id=$1 ORDER BY metric, destination`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var routes []models.Route
	for rows.Next() {
		var r models.Route
		if err := rows.Scan(&r.Destination, &r.Gateway, &r.InterfaceName, &r.Metric, &r.Flags); err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, rows.Err()
}

// MetricsHistory returns raw metric rows for an interface in a time window.
func MetricsHistory(ctx context.Context, pool *pgxpool.Pool, ifaceID string, from, to time.Time) (pgx.Rows, error) {
	return pool.Query(ctx, `
		SELECT time, bytes_recv, bytes_sent, packets_recv, packets_sent,
		       errors_in, errors_out, drops_in, drops_out
		FROM metrics_raw
		WHERE interface_id=$1 AND time BETWEEN $2 AND $3
		ORDER BY time`,
		ifaceID, from, to)
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
