package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pedro-mueller/conduit/internal/breaker"
	"github.com/pedro-mueller/conduit/internal/capture"
	"github.com/pedro-mueller/conduit/internal/config"
	"github.com/pedro-mueller/conduit/internal/metrics"
	"github.com/pedro-mueller/conduit/internal/notify"
	"github.com/pedro-mueller/conduit/internal/proxy"
	"github.com/pedro-mueller/conduit/internal/route"
)

func main() {
	configPath := flag.String("config", "", "path to config.toml (default: ~/.config/conduit/config.toml)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conduit: %v\n", err)
		os.Exit(1)
	}

	log := newLogger(cfg.Log.Level)

	notify.SetEnabledFromEnv()

	br := breaker.New(cfg.Paths.StatePath, cfg.FallbackOpenDuration(), cfg.Breaker.ProbeOnExpiry, func(format string, args ...any) {
		log.Warn(fmt.Sprintf(format, args...))
	})
	br.SetFailoverProvider(cfg.Breaker.FailoverProvider)
	cap := capture.New(cfg.Log.CapturePath, cfg.Log.CaptureUpstreamErrors)
	met := metrics.New()
	router := route.New(cfg.Jev, cfg.TypeSafeAPIKey, log)
	gw := proxy.New(cfg, br, cap, met, router, log)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conduit: listen %s: %v\n", cfg.Listen, err)
		os.Exit(1)
	}
	// Defense in depth: refuse non-loopback binds even if config check is bypassed.
	if addr, ok := ln.Addr().(*net.TCPAddr); ok && addr.IP != nil && !addr.IP.IsLoopback() {
		fmt.Fprintf(os.Stderr, "conduit: refusing non-loopback bind %s\n", ln.Addr())
		os.Exit(1)
	}

	srv := &http.Server{
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}

	log.Info("gateway listening",
		"listen", cfg.Listen,
		"anthropic", cfg.Anthropic.BaseURL,
		"glm", cfg.GLM.BaseURL,
		"deepseek_enabled", cfg.DeepSeekAPIKey != "",
		"jev_enabled", router.Enabled(),
		"mode", string(br.Mode()),
		"state", cfg.Paths.StatePath,
	)

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	return slog.New(h)
}
