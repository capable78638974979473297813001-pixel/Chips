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
	"sync"
	"syscall"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
	"github.com/capable78638974979473297813001-pixel/chips/internal/server"
	"github.com/capable78638974979473297813001-pixel/chips/internal/tlsconf"
)

func logReload(log *slog.Logger, trigger string, changes []string, err error) {
	if err != nil {
		log.Error("config reload failed; previous config still in use", "trigger", trigger, "error", err)
		return
	}
	if len(changes) == 0 {
		log.Info("config reloaded: no changes", "trigger", trigger)
	}
	for _, c := range changes {
		log.Info("config reloaded", "trigger", trigger, "change", c)
	}
}

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
	checkConfig := flag.Bool("check-config", false, "validate the config file (and its environment variables) and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *checkConfig {
		cfg, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		fmt.Printf("%s: OK (%d backends, %d discovery sources)\n", *configPath, len(cfg.Backends), len(cfg.Discovery))
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

	p, err := pool.New(cfg.Backends, pool.OptionsFromConfig(cfg), log)
	if err != nil {
		return err
	}
	var certs *tlsconf.Store
	if cfg.TLS.Enabled() {
		if certs, err = tlsconf.Load(cfg.TLS); err != nil {
			return err
		}
	}

	maxConns := 0
	for _, b := range cfg.Backends {
		maxConns = max(maxConns, b.MaxConcurrency)
	}
	for _, d := range cfg.Discovery {
		maxConns = max(maxConns, d.Backend.MaxConcurrency)
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

	if cfg.Auth.Enabled() && len(cfg.Auth.AdminKeys) == 0 {
		log.Warn("no admin keys configured: every client key can use /admin/* (see auth.admin_keys)")
	}
	router := server.New(cfg, p, client, log)
	router.DiscoverNow(ctx) // discovered backends join the first health checks
	go router.RunDiscovery(ctx)

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
	}
	go agents.Run(ctx) // also covers agents that a reload or discovery adds later
	go (&pool.EngineScraper{
		Pool:     p,
		Client:   client,
		Interval: cfg.EngineMetrics.Interval.D(),
		Timeout:  cfg.EngineMetrics.Timeout.D(),
	}).Run(ctx)
	for _, st := range p.Snapshot() {
		attrs := []any{"name", st.Name, "url", st.URL, "tier", st.Tier, "tier_source", st.TierSource,
			"healthy", st.Healthy, "max_concurrency", st.MaxConcurrency, "models", st.Models}
		if hw := st.Hardware; hw != nil {
			attrs = append(attrs, "accelerators", hw.Accelerators, "vendors", hw.Vendors,
				"accelerator_memory_gib", hw.MemoryTotalBytes>>30)
		}
		log.Info("backend", attrs...)
	}

	go router.RunCluster(ctx)
	go router.RunTracing(ctx)
	if dir := cfg.StateDir; dir != "" {
		cached, err := router.LoadState(dir)
		if err != nil {
			log.Warn("could not restore saved state; starting fresh", "dir", dir, "error", err)
		} else {
			log.Info("restored saved state", "dir", dir, "cached_responses", cached)
		}
		go func() {
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := router.SaveState(dir); err != nil {
						log.Warn("saving state failed", "dir", dir, "error", err)
					}
				}
			}
		}()
	}
	if cfg.Cluster.Enabled() {
		log.Info("cluster mode", "node", cfg.Cluster.NodeName, "peers", cfg.Cluster.Peers)
	}
	// reload re-reads the config file and applies it; on any error the
	// running config stays in place.
	var reloadMu sync.Mutex
	reload := func() ([]string, error) {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		next, err := config.Load(*configPath)
		if err != nil {
			return nil, err
		}
		if certs != nil && next.TLS.Enabled() {
			if err := certs.Reload(next.TLS); err != nil {
				return nil, err
			}
		}
		return router.Apply(next)
	}
	router.SetReloader(func() ([]string, error) {
		changes, err := reload()
		logReload(log, "admin API", changes, err)
		return changes, err
	})
	go func() {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				changes, err := reload()
				logReload(log, "SIGHUP", changes, err)
			}
		}
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No write timeout: streamed generations can legitimately run for minutes.
	}
	// HTTP/1.1 and HTTP/2 everywhere, including HTTP/2 without TLS (h2c),
	// which plaintext gRPC clients use.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Protocols = &protocols
	errc := make(chan error, 1)
	if certs != nil {
		srv.TLSConfig = certs.ServerConfig()
		go certs.Watch(ctx, 30*time.Second, log)
		go func() {
			log.Info("siliconrouter listening", "addr", cfg.Listen, "tls", true, "version", version)
			errc <- srv.ListenAndServeTLS("", "")
		}()
	} else {
		go func() {
			log.Info("siliconrouter listening", "addr", cfg.Listen, "tls", false, "version", version)
			errc <- srv.ListenAndServe()
		}()
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down; draining in-flight requests")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if dir := cfg.StateDir; dir != "" {
		if err := router.SaveState(dir); err != nil {
			log.Error("saving state failed", "dir", dir, "error", err)
		} else {
			log.Info("saved state", "dir", dir)
		}
	}
	if shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
		return shutdownErr
	}
	return nil
}
