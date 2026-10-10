// Command siliconrouter runs the SiliconRouter inference load balancer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/hardware"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
	"github.com/capable78638974979473297813001-pixel/chips/internal/server"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "siliconrouter:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultConfig := os.Getenv("SILICONROUTER_CONFIG")
	if defaultConfig == "" {
		defaultConfig = "siliconrouter.yaml"
	}
	configPath := flag.String("config", defaultConfig, "path to the YAML config file (env SILICONROUTER_CONFIG)")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	logJSON := flag.Bool("log-json", false, "emit JSON logs")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid -log-level: %w", err)
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	log := slog.New(handler)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	p, err := pool.New(cfg.Backends, pool.Options{
		MaxWaiting:           cfg.Queue.MaxWaiting,
		AffinityMaxImbalance: cfg.Routing.AffinityMaxImbalance,
		LargeRequestTokens:   cfg.Routing.LargeRequestTokens,
		UnhealthyThreshold:   cfg.Health.UnhealthyThreshold,
		HealthyThreshold:     cfg.Health.HealthyThreshold,
		PassiveHealth:        !cfg.Health.Disabled,
		Tiers: hardware.Thresholds{
			HeavyMinBytes:    uint64(cfg.Hardware.HeavyMinMemoryGiB * (1 << 30)),
			StandardMinBytes: uint64(cfg.Hardware.StandardMinMemoryGiB * (1 << 30)),
		},
	}, log)
	if err != nil {
		return err
	}

	maxConns := 0
	for _, b := range cfg.Backends {
		maxConns = max(maxConns, b.MaxConcurrency)
	}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: cfg.Upstream.DialTimeout.D(), KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   maxConns,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: cfg.Upstream.ResponseHeaderTimeout.D(),
		ForceAttemptHTTP2:     true,
	}
	client := &http.Client{Transport: transport}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !cfg.Health.Disabled {
		checker := &pool.Checker{
			Pool:     p,
			Client:   client,
			Path:     cfg.Health.Path,
			Interval: cfg.Health.Interval.D(),
			Timeout:  cfg.Health.Timeout.D(),
		}
		checker.CheckAll(ctx) // know who is up before taking traffic
		go checker.Run(ctx)
	}
	agents := &pool.AgentPoller{
		Pool:     p,
		Client:   client,
		Interval: cfg.Hardware.PollInterval.D(),
		Timeout:  cfg.Hardware.Timeout.D(),
	}
	if agents.HasAgents() {
		agents.PollAll(ctx) // tier auto backends before taking traffic
		go agents.Run(ctx)
	}
	for _, st := range p.Snapshot() {
		attrs := []any{"name", st.Name, "url", st.URL, "tier", st.Tier, "tier_source", st.TierSource,
			"healthy", st.Healthy, "max_concurrency", st.MaxConcurrency, "models", st.Models}
		if hw := st.Hardware; hw != nil {
			attrs = append(attrs, "accelerators", hw.Accelerators, "vendors", hw.Vendors,
				"accelerator_memory_gib", hw.MemoryTotalBytes>>30)
		}
		log.Info("backend", attrs...)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(cfg, p, client, log),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No write timeout: streamed generations can legitimately run for minutes.
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("siliconrouter listening", "addr", cfg.Listen, "version", version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down; draining in-flight requests")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}
