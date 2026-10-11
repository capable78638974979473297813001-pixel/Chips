// Package tlsconf serves the router over HTTPS from certificate files that
// may be replaced at any time.
package tlsconf

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

// Store holds the current TLS configuration built from files.
type Store struct {
	mu      sync.Mutex // serializes reloads
	cfg     config.TLS
	stamps  map[string]time.Time
	current atomic.Pointer[tls.Config]
}

// Load reads the certificate, key and optional client CA files.
func Load(cfg config.TLS) (*Store, error) {
	s := &Store{}
	if err := s.Reload(cfg); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload re-reads the files named by cfg (the paths may change too). On
// error the previous configuration stays in use.
func (s *Store) Reload(cfg config.TLS) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked(cfg)
}

func (s *Store) reloadLocked(cfg config.TLS) error {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("tls: load certificate: %w", err)
	}
	tc := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	if cfg.MinVersion == "1.3" {
		tc.MinVersion = tls.VersionTLS13
	}
	if cfg.ClientCAFile != "" {
		pem, err := os.ReadFile(cfg.ClientCAFile)
		if err != nil {
			return fmt.Errorf("tls: read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return errors.New("tls: client_ca_file has no PEM certificates")
		}
		tc.ClientCAs = pool
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	}
	s.cfg = cfg
	s.stamps = stamps(cfg)
	s.current.Store(tc)
	return nil
}

func stamps(cfg config.TLS) map[string]time.Time {
	out := map[string]time.Time{}
	for _, f := range []string{cfg.CertFile, cfg.KeyFile, cfg.ClientCAFile} {
		if f == "" {
			continue
		}
		if fi, err := os.Stat(f); err == nil {
			out[f] = fi.ModTime()
		}
	}
	return out
}

// ReloadIfChanged reloads when any file's modification time has changed.
func (s *Store) ReloadIfChanged() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := stamps(s.cfg)
	changed := len(now) != len(s.stamps)
	for f, t := range now {
		if !s.stamps[f].Equal(t) {
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	return true, s.reloadLocked(s.cfg)
}

// ServerConfig is the tls.Config for http.Server. Each new connection gets
// the latest certificates.
func (s *Store) ServerConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return s.current.Load(), nil
		},
	}
}

// Watch checks the files every interval until ctx ends, reloading changes.
func (s *Store) Watch(ctx context.Context, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if changed, err := s.ReloadIfChanged(); err != nil {
				log.Error("TLS certificate reload failed; keeping the previous one", "error", err)
			} else if changed {
				log.Info("TLS certificate reloaded")
			}
		}
	}
}
