package hub

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/neteye/center/internal/models"
	"github.com/neteye/center/internal/topology"
)

var frontendUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 16384,
	CheckOrigin:    func(r *http.Request) bool { return true },
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

	mu      sync.RWMutex
	clients map[*frontendConn]struct{}
}

// NewFrontendHub creates a FrontendHub. Call Run() in a goroutine.
func NewFrontendHub(topo *topology.Store) *FrontendHub {
	return &FrontendHub{
		topo:    topo,
		clients: make(map[*frontendConn]struct{}),
	}
}

// ServeHTTP handles a frontend WebSocket upgrade.
func (h *FrontendHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := frontendUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("frontend upgrade error: %v", err)
		return
	}

	fc := &frontendConn{conn: conn, send: make(chan []byte, 256)}

	h.mu.Lock()
	h.clients[fc] = struct{}{}
	h.mu.Unlock()

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
		fc.conn.Close()
		h.mu.Lock()
		delete(h.clients, fc)
		h.mu.Unlock()
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
		log.Printf("frontend broadcast marshal: %v", err)
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for fc := range h.clients {
		select {
		case fc.send <- data:
		default:
			// Client send buffer full — drop this message for this client.
		}
	}
}

// ClientCount returns the number of connected frontend clients.
func (h *FrontendHub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
