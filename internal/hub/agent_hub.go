// Package hub manages persistent WebSocket connections from agents and frontend clients.
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neteye/center/internal/config"
	"github.com/neteye/center/internal/db"
	"github.com/neteye/center/internal/models"
	"github.com/neteye/center/internal/topology"
)

var agentUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(_ *http.Request) bool { return true },
}

// agentConn holds per-connection state for a connected agent.
type agentConn struct {
	conn     *websocket.Conn
	deviceID string
	hostname string
	mu       sync.Mutex // guards conn writes
}

// AgentHub accepts WebSocket connections from agents, processes their updates,
// and notifies the FrontendHub of topology changes.
type AgentHub struct {
	cfg         *config.Config
	pool        *pgxpool.Pool
	topo        *topology.Store
	frontendHub *FrontendHub
	log         *slog.Logger

	mu     sync.RWMutex
	agents map[string]*agentConn // deviceID → conn

	// prevMetrics holds the last raw counter values per interface for rate calculation.
	prevMu      sync.Mutex
	prevMetrics map[string]prevSample // "<deviceID>/<ifaceName>" → sample
}

type prevSample struct {
	t       time.Time
	metrics models.InterfaceMetrics
}

// NewAgentHub creates an AgentHub.
func NewAgentHub(cfg *config.Config, pool *pgxpool.Pool, topo *topology.Store, fh *FrontendHub, log *slog.Logger) *AgentHub {
	return &AgentHub{
		cfg:         cfg,
		pool:        pool,
		topo:        topo,
		frontendHub: fh,
		log:         log,
		agents:      make(map[string]*agentConn),
		prevMetrics: make(map[string]prevSample),
	}
}

// ServeHTTP handles an incoming agent WebSocket upgrade.
func (h *AgentHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := agentUpgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Warn("upgrade error", "err", err, "remote", r.RemoteAddr)
		return
	}
	go h.handleAgent(conn)
}

func (h *AgentHub) handleAgent(conn *websocket.Conn) {
	defer conn.Close() //nolint:errcheck

	ac := &agentConn{conn: conn}
	ctx := context.Background()

	// Set initial read deadline for the registration message.
	conn.SetReadDeadline(time.Now().Add(15 * time.Second)) //nolint:errcheck

	// First message must be a registration.
	_, data, err := conn.ReadMessage()
	if err != nil {
		h.log.Warn("registration read error", "err", err)
		return
	}
	var msg models.AgentMessage
	if err := json.Unmarshal(data, &msg); err != nil || msg.Type != "register" || msg.Register == nil {
		h.log.Warn("invalid registration message", "err", err)
		return
	}

	reg := msg.Register
	deviceID, err := db.UpsertDevice(ctx, h.pool, reg)
	if err != nil {
		h.log.Error("upsert device", "hostname", reg.Hostname, "err", err)
		return
	}
	ac.deviceID = deviceID
	ac.hostname = reg.Hostname

	h.log.Info("agent connected",
		"hostname", reg.Hostname,
		"device_id", deviceID,
		"os", reg.OS,
		"arch", reg.Arch,
		"version", reg.AgentVersion)

	h.mu.Lock()
	h.agents[deviceID] = ac
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.agents, deviceID)
		h.mu.Unlock()

		if err := db.MarkDeviceOffline(ctx, h.pool, deviceID); err != nil {
			h.log.Error("mark device offline", "hostname", reg.Hostname, "device_id", deviceID, "err", err)
		}
		h.topo.SetOffline(deviceID)
		h.frontendHub.BroadcastJSON(models.FrontendMessage{
			Type: "device_offline",
			DeviceOffline: &models.DeviceOfflineMsg{
				DeviceID: deviceID,
				Hostname: reg.Hostname,
				LastSeen: time.Now(),
			},
		})
		h.log.Info("agent disconnected", "hostname", reg.Hostname, "device_id", deviceID)
	}()

	// Remove read deadline; subsequent messages are heartbeat-driven.
	conn.SetReadDeadline(time.Time{}) //nolint:errcheck
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(h.cfg.Server.OfflineTimeout * 2)) //nolint:errcheck
		return nil
	})

	// Background pinger to detect dead connections.
	go func() {
		ticker := time.NewTicker(h.cfg.Server.OfflineTimeout / 2)
		defer ticker.Stop()
		for range ticker.C {
			ac.mu.Lock()
			err := conn.WriteMessage(websocket.PingMessage, nil)
			ac.mu.Unlock()
			if err != nil {
				conn.Close() //nolint:errcheck
				return
			}
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(h.cfg.Server.OfflineTimeout * 2)) //nolint:errcheck

		var m models.AgentMessage
		if err := json.Unmarshal(data, &m); err != nil {
			h.log.Warn("bad message from agent", "hostname", reg.Hostname, "err", err)
			continue
		}
		if m.Type == "update" && m.Update != nil {
			h.processUpdate(ctx, ac, m.Update)
		}
	}
}

func (h *AgentHub) processUpdate(ctx context.Context, ac *agentConn, u *models.AgentUpdate) {
	// Upsert interfaces + addresses in DB.
	ifaceIDs, err := db.UpsertInterfaces(ctx, h.pool, ac.deviceID, u.Interfaces)
	if err != nil {
		h.log.Error("upsert interfaces", "hostname", ac.hostname, "err", err)
		return
	}

	// Replace routing table.
	if err := db.ReplaceRoutes(ctx, h.pool, ac.deviceID, u.Routes); err != nil {
		h.log.Error("replace routes", "hostname", ac.hostname, "err", err)
	}

	// Insert raw metrics and compute rates.
	var metricsUpdates []models.MetricsUpdate
	for _, iface := range u.Interfaces {
		id, ok := ifaceIDs[iface.Name]
		if !ok {
			continue
		}
		if err := db.InsertRawMetrics(ctx, h.pool, u.Timestamp, id, iface.Metrics); err != nil {
			h.log.Error("insert raw metrics", "hostname", ac.hostname, "iface", iface.Name, "err", err)
		}
		if rate, ok := h.computeRate(ac.deviceID, iface.Name, u.Timestamp, iface.Metrics); ok {
			metricsUpdates = append(metricsUpdates, rate)
		}
	}

	h.log.Debug("processed update",
		"hostname", ac.hostname,
		"interfaces", len(u.Interfaces),
		"routes", len(u.Routes),
		"metrics_rates", len(metricsUpdates))

	// Update in-memory topology store.
	deviceInfo := h.buildDeviceInfo(ac, u)
	h.topo.Upsert(deviceInfo)

	// Push changes to all frontend clients.
	h.frontendHub.BroadcastJSON(models.FrontendMessage{
		Type:         "device_update",
		DeviceUpdate: &deviceInfo,
	})
	for i := range metricsUpdates {
		h.frontendHub.BroadcastJSON(models.FrontendMessage{
			Type:    "metrics",
			Metrics: &metricsUpdates[i],
		})
	}
}

func (h *AgentHub) computeRate(deviceID, ifaceName string, t time.Time, cur models.InterfaceMetrics) (models.MetricsUpdate, bool) {
	key := fmt.Sprintf("%s/%s", deviceID, ifaceName)
	h.prevMu.Lock()
	prev, hasPrev := h.prevMetrics[key]
	h.prevMetrics[key] = prevSample{t: t, metrics: cur}
	h.prevMu.Unlock()

	if !hasPrev {
		return models.MetricsUpdate{}, false
	}
	dt := t.Sub(prev.t).Seconds()
	if dt <= 0 {
		return models.MetricsUpdate{}, false
	}
	rate := func(cur, prev uint64) float64 {
		if cur < prev {
			return 0 // counter wrap
		}
		return float64(cur-prev) / dt
	}
	return models.MetricsUpdate{
		DeviceID:        deviceID,
		InterfaceName:   ifaceName,
		Timestamp:       t,
		BytesRecvRate:   rate(cur.BytesRecv, prev.metrics.BytesRecv),
		BytesSentRate:   rate(cur.BytesSent, prev.metrics.BytesSent),
		PacketsRecvRate: rate(cur.PacketsRecv, prev.metrics.PacketsRecv),
		PacketsSentRate: rate(cur.PacketsSent, prev.metrics.PacketsSent),
		ErrorsInRate:    rate(cur.ErrorsIn, prev.metrics.ErrorsIn),
		ErrorsOutRate:   rate(cur.ErrorsOut, prev.metrics.ErrorsOut),
		DropsInRate:     rate(cur.DropsIn, prev.metrics.DropsIn),
		DropsOutRate:    rate(cur.DropsOut, prev.metrics.DropsOut),
	}, true
}

func (h *AgentHub) buildDeviceInfo(ac *agentConn, u *models.AgentUpdate) models.DeviceInfo {
	existing := h.topo.Get(ac.deviceID)
	ifaces := u.Interfaces
	if ifaces == nil {
		ifaces = []models.InterfaceInfo{}
	}
	for i := range ifaces {
		if ifaces[i].Addresses == nil {
			ifaces[i].Addresses = []models.AddressInfo{}
		}
	}
	routes := u.Routes
	if routes == nil {
		routes = []models.Route{}
	}
	d := models.DeviceInfo{
		ID:         ac.deviceID,
		Hostname:   ac.hostname,
		Status:     "online",
		LastSeen:   u.Timestamp,
		Interfaces: ifaces,
		Routes:     routes,
	}
	if existing != nil {
		d.FirstSeen = existing.FirstSeen
		d.OS = existing.OS
		d.Arch = existing.Arch
	} else {
		d.FirstSeen = u.Timestamp
	}
	return d
}

// ConnectedCount returns the number of currently connected agents.
func (h *AgentHub) ConnectedCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.agents)
}
