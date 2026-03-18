package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"time"
	"os/signal"
	"syscall"

	"github.com/neteye/center/internal/api"
	"github.com/neteye/center/internal/config"
	"github.com/neteye/center/internal/db"
	"github.com/neteye/center/internal/hub"
	"github.com/neteye/center/internal/retention"
	"github.com/neteye/center/internal/topology"
)

func main() {
	cfgPath := flag.String("config", "", "path to YAML config file (optional)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ── Database ─────────────────────────────────────────────────────────
	pool, err := db.Connect(ctx, cfg.Database)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer pool.Close()
	log.Println("database connected and migrated")

	// ── Topology store — seed from DB so restart preserves history ────────
	topo := topology.NewStore()
	devices, err := db.LoadAllDevices(ctx, pool)
	if err != nil {
		log.Printf("warn: could not load devices from DB: %v", err)
	} else {
		topo.LoadFromDB(devices)
		log.Printf("topology: loaded %d devices from DB", len(devices))
	}

	// ── Hubs ─────────────────────────────────────────────────────────────
	frontendHub := hub.NewFrontendHub(topo)
	agentHub := hub.NewAgentHub(cfg, pool, topo, frontendHub)

	// ── Retention manager ────────────────────────────────────────────────
	retMgr := retention.New(pool, cfg.Retention)
	go retMgr.Run(ctx)

	// ── Agent WebSocket server ────────────────────────────────────────────
	agentMux := http.NewServeMux()
	agentMux.Handle("/ws", agentHub)
	agentMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok")) //nolint:errcheck
	})
	agentServer := &http.Server{Addr: cfg.Server.AgentAddr, Handler: agentMux}

	go func() {
		log.Printf("agent server listening on %s", cfg.Server.AgentAddr)
		if err := agentServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("agent server: %v", err)
		}
	}()

	// ── Frontend HTTP + WebSocket server ─────────────────────────────────
	frontendServer := &http.Server{
		Addr:    cfg.Server.FrontendAddr,
		Handler: api.Handler(pool, topo, frontendHub),
	}

	go func() {
		log.Printf("frontend server listening on %s", cfg.Server.FrontendAddr)
		if err := frontendServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("frontend server: %v", err)
		}
	}()

	log.Printf("neteye-center running | agents→%s | frontend→%s",
		cfg.Server.AgentAddr, cfg.Server.FrontendAddr)

	// ── Graceful shutdown ─────────────────────────────────────────────────
	<-ctx.Done()
	log.Println("shutting down...")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	agentServer.Shutdown(shutCtx)    //nolint:errcheck
	frontendServer.Shutdown(shutCtx) //nolint:errcheck
	log.Println("stopped")
}
