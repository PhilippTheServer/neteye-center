package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/neteye/center/internal/models"
	"github.com/neteye/center/internal/topology"
)

var frontendUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 16384,
	CheckOrigin:     func(_ *http.Request) bool { return true },
}

type frontendConn struct {
	conn *websocket.Conn
	send chan []byte
}

// FrontendHub manages WebSocket connections from Angular frontend clients.
// On connect, it sends a full topology snapshot; then broadcasts incremental
// updates as devices report in.
type FrontendHub struct {
	topo *topology.Store
	log  *slog.Logger

	mu      sync.RWMutex
	clients map[*frontendConn]struct{}
}

// NewFrontendHub creates a FrontendHub.
func NewFrontendHub(topo *topology.Store, log *slog.Logger) *FrontendHub {
	return &FrontendHub{
		topo:    topo,
		log:     log,
		clients: make(map[*frontendConn]struct{}),
	}
}

// ServeHTTP handles a frontend WebSocket upgrade.
func (h *FrontendHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := frontendUpgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Warn("upgrade error", "err", err, "remote", r.RemoteAddr)
		return
	}

	fc := &frontendConn{conn: conn, send: make(chan []byte, 256)}

	h.mu.Lock()
	h.clients[fc] = struct{}{}
	clientCount := len(h.clients)
	h.mu.Unlock()

	h.log.Debug("frontend client connected", "remote", r.RemoteAddr, "total_clients", clientCount)

	// Send initial topology snapshot.
	snap := h.topo.Snapshot()
	if data, err := json.Marshal(models.FrontendMessage{
		Type:     "topology",
		Topology: &snap,
	}); err == nil {
		fc.send <- data
	}

	go h.writePump(fc)
	go h.readPump(fc)
}

func (h *FrontendHub) writePump(fc *frontendConn) {
	defer func() {
		fc.conn.Close() //nolint:errcheck
		h.mu.Lock()
		delete(h.clients, fc)
		clientCount := len(h.clients)
		h.mu.Unlock()
		h.log.Debug("frontend client disconnected", "total_clients", clientCount)
	}()
	for data := range fc.send {
		if err := fc.conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
	}
}

func (h *FrontendHub) readPump(fc *frontendConn) {
	// We don't expect messages from the frontend over this channel.
	// Just drain to detect disconnects.
	defer close(fc.send)
	for {
		if _, _, err := fc.conn.ReadMessage(); err != nil {
			return
		}
	}
}

// BroadcastJSON encodes msg as JSON and sends it to all connected frontend clients.
func (h *FrontendHub) BroadcastJSON(msg models.FrontendMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		h.log.Error("broadcast marshal error", "err", err)
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for fc := range h.clients {
		select {
		case fc.send <- data:
		default:
			// Client send buffer full — drop this message for this client.
			h.log.Warn("client send buffer full, dropping message", "msg_type", msg.Type)
		}
	}
}

// ClientCount returns the number of connected frontend clients.
func (h *FrontendHub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
