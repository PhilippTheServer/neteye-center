// Package api wires up all HTTP handlers for the frontend-facing server.
package api //nolint:revive // "api" is an intentional, well-understood package name

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neteye/center/internal/db"
	"github.com/neteye/center/internal/hub"
	"github.com/neteye/center/internal/models"
	"github.com/neteye/center/internal/topology"
)

// Handler returns an http.Handler for the frontend-facing API.
// Routes:
//
//	GET  /ws            → WebSocket stream of FrontendMessages
//	GET  /api/topology  → current snapshot (JSON)
//	GET  /api/devices   → list devices
//	GET  /api/route     → route analysis (?src=<id>&dst=<id>)
//	GET  /api/metrics   → metrics history (?iface=<id>&from=<t>&to=<t>)
//	GET  /health        → liveness probe
func Handler(pool *pgxpool.Pool, topo *topology.Store, fh *hub.FrontendHub, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	// WebSocket endpoint for the Angular frontend.
	mux.Handle("/ws", fh)

	// REST endpoints.
	mux.HandleFunc("/api/topology", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, topo.Snapshot())
	})

	mux.HandleFunc("/api/devices", func(w http.ResponseWriter, r *http.Request) {
		devices, err := db.LoadAllDevices(r.Context(), pool)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, devices)
	})

	mux.HandleFunc("/api/route", func(w http.ResponseWriter, r *http.Request) {
		src := r.URL.Query().Get("src")
		dst := r.URL.Query().Get("dst")
		if src == "" || dst == "" {
			http.Error(w, "src and dst query params required", http.StatusBadRequest)
			return
		}
		result := topo.AnalyzeRoute(src, dst)
		writeJSON(w, result)
	})

	mux.HandleFunc("/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		ifaceID := r.URL.Query().Get("iface")
		fromStr := r.URL.Query().Get("from")
		toStr := r.URL.Query().Get("to")
		if ifaceID == "" {
			http.Error(w, "iface param required", http.StatusBadRequest)
			return
		}
		from := time.Now().Add(-1 * time.Hour)
		to := time.Now()
		if fromStr != "" {
			if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
				from = t
			}
		}
		if toStr != "" {
			if t, err := time.Parse(time.RFC3339, toStr); err == nil {
				to = t
			}
		}

		rows, err := db.MetricsHistory(r.Context(), pool, ifaceID, from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		type row struct {
			Time        time.Time `json:"time"`
			BytesRecv   uint64    `json:"bytes_recv"`
			BytesSent   uint64    `json:"bytes_sent"`
			PacketsRecv uint64    `json:"packets_recv"`
			PacketsSent uint64    `json:"packets_sent"`
			ErrorsIn    uint64    `json:"errors_in"`
			ErrorsOut   uint64    `json:"errors_out"`
			DropsIn     uint64    `json:"drops_in"`
			DropsOut    uint64    `json:"drops_out"`
		}
		var result []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.Time, &r.BytesRecv, &r.BytesSent, &r.PacketsRecv, &r.PacketsSent,
				&r.ErrorsIn, &r.ErrorsOut, &r.DropsIn, &r.DropsOut); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			result = append(result, r)
		}
		writeJSON(w, result)
	})

	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, _ *http.Request) {
		snap := topo.Snapshot()
		online := 0
		for _, d := range snap.Devices {
			if d.Status == "online" {
				online++
			}
		}
		writeJSON(w, map[string]interface{}{
			"total_devices":    len(snap.Devices),
			"online_devices":   online,
			"frontend_clients": fh.ClientCount(),
		})
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, models.FrontendMessage{Type: "ok"})
	})

	return requestLogger(log, corsMiddleware(mux))
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// corsMiddleware allows the Angular dev server (any origin) to call the API.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestLogger logs each HTTP request at Debug level with method, path, status, and duration.
func requestLogger(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip WebSocket upgrade logging — the hub handles that.
		if r.Header.Get("Upgrade") == "websocket" {
			next.ServeHTTP(w, r)
			return
		}
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rw, r)
		log.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration", time.Since(start).Round(time.Microsecond),
			"remote", r.RemoteAddr,
		)
	})
}

// statusWriter wraps ResponseWriter to capture the written status code.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}
