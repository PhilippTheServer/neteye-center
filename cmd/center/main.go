// Package main is the entry point for the neteye-center server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/neteye/center/internal/api"
	"github.com/neteye/center/internal/config"
	"github.com/neteye/center/internal/db"
	"github.com/neteye/center/internal/hub"
	"github.com/neteye/center/internal/retention"
	"github.com/neteye/center/internal/topology"
)

func main() {
	cfgPath := flag.String("config", "", "path to YAML config file (optional)")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	logFormat := flag.String("log-format", "text", "log format: text, json")
	flag.Parse()

	logger := buildLogger(*logLevel, *logFormat)
	slog.SetDefault(logger)

	if err := run(logger, *cfgPath); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ── Database ─────────────────────────────────────────────────────────
	pool, err := db.Connect(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()
	logger.Info("database connected and migrated")

	// ── Topology store — seed from DB so restart preserves history ────────
	topo := topology.NewStore()
	devices, err := db.LoadAllDevices(ctx, pool)
	if err != nil {
		logger.Warn("could not load devices from DB", "err", err)
	} else {
		topo.LoadFromDB(devices)
		logger.Info("topology seeded from DB", "devices", len(devices))
	}

	// ── Hubs ─────────────────────────────────────────────────────────────
	frontendHub := hub.NewFrontendHub(topo, logger.With("component", "frontend_hub"))
	agentHub := hub.NewAgentHub(cfg, pool, topo, frontendHub, logger.With("component", "agent_hub"))

	// ── Retention manager ────────────────────────────────────────────────
	retMgr := retention.New(pool, cfg.Retention, logger.With("component", "retention"))
	go retMgr.Run(ctx)

	// ── Agent WebSocket server ────────────────────────────────────────────
	agentMux := http.NewServeMux()
	agentMux.Handle("/ws", agentHub)
	agentMux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok")) //nolint:errcheck
	})
	agentServer := &http.Server{Addr: cfg.Server.AgentAddr, Handler: agentMux}

	go func() {
		logger.Info("agent server listening", "addr", cfg.Server.AgentAddr)
		if err := agentServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("agent server", "err", err)
		}
	}()

	// ── Frontend HTTP + WebSocket server ─────────────────────────────────
	frontendServer := &http.Server{
		Addr:    cfg.Server.FrontendAddr,
		Handler: api.Handler(pool, topo, frontendHub, logger.With("component", "api")),
	}

	go func() {
		logger.Info("frontend server listening", "addr", cfg.Server.FrontendAddr)
		if err := frontendServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("frontend server", "err", err)
		}
	}()

	logger.Info("neteye-center running",
		"agent_addr", cfg.Server.AgentAddr,
		"frontend_addr", cfg.Server.FrontendAddr)

	// ── Graceful shutdown ─────────────────────────────────────────────────
	<-ctx.Done()
	logger.Info("shutting down")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	agentServer.Shutdown(shutCtx)    //nolint:errcheck
	frontendServer.Shutdown(shutCtx) //nolint:errcheck
	logger.Info("stopped")
	return nil
}

func buildLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
