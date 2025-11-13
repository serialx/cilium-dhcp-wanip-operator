package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"serialx.net/cilium-dhcp-wanip-operator/internal/agent"
)

func main() {
	var (
		listenAddr = flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
		stateDir   = flag.String("state-dir", "/var/lib/dhcp-wan-agent", "State directory for lease persistence")
		logLevel   = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	)
	flag.Parse()

	// Configure logger
	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)

	// Create agent
	cfg := &agent.Config{
		ListenAddr: *listenAddr,
		StateDir:   *stateDir,
		Logger:     logger,
	}

	a, err := agent.New(cfg)
	if err != nil {
		logger.Error("failed to create agent", "error", err)
		os.Exit(1)
	}

	// Start agent
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		logger.Error("failed to start agent", "error", err)
		os.Exit(1)
	}

	logger.Info("DHCP WAN agent started", "listen", *listenAddr)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("received shutdown signal")

	// Graceful shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := a.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown failed", "error", err)
		os.Exit(1)
	}

	logger.Info("agent stopped")
}
