package server

import (
	"encoding/json"
	"net/http"
	"reflect"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
)

// Apply switches the router to cfg without dropping traffic: backends,
// keys, limits, priorities, aliases, cache, queue, retry and routing
// settings take effect for new requests at once, while in-flight requests
// finish under the old settings. It returns a description of each change,
// including settings that only take effect after a restart.
func (s *Server) Apply(cfg *config.Config) ([]string, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	old := s.state.Load()
	changes, err := s.pool.Update(cfg.Backends, pool.OptionsFromConfig(cfg))
	if err != nil {
		return nil, err
	}
	s.state.Store(newState(cfg, old))
	s.limits.SetLimits(limitsFrom(cfg))
	return append(changes, settingChanges(old.cfg, cfg)...), nil
}

// SetReloader installs the function POST /admin/reload calls to re-read
// the configuration (the caller owns the file and the TLS certificates).
func (s *Server) SetReloader(fn func() ([]string, error)) { s.reloader.Store(&fn) }

func (s *Server) handleReload(w http.ResponseWriter, _ *http.Request) {
	fn := s.reloader.Load()
	if fn == nil {
		writeError(w, http.StatusNotImplemented, "reload_unavailable", "this router was not started from a config file")
		return
	}
	changes, err := (*fn)()
	if err != nil {
		writeError(w, http.StatusBadRequest, "reload_failed", "config not applied, previous config still in use: "+err.Error())
		return
	}
	if changes == nil {
		changes = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "applied", "changes": changes})
}

// settingChanges describes what differs between two configs outside the
// backend list, which pool.Update reports.
func settingChanges(old, cfg *config.Config) []string {
	var out []string
	changed := func(a, b any, what string) {
		if !reflect.DeepEqual(a, b) {
			out = append(out, what)
		}
	}
	changed(old.Auth, cfg.Auth, "updated API keys, admin keys or limits")
	changed(old.Models, cfg.Models, "updated model aliases")
	changed(old.Cache, cfg.Cache, "updated response cache settings")
	changed(old.Queue, cfg.Queue, "updated queue settings")
	changed(old.Retry, cfg.Retry, "updated retry settings")
	changed(old.MaxBodyBytes, cfg.MaxBodyBytes, "updated max_body_bytes")
	changed([]any{old.Hardware.HeavyMinMemoryGiB, old.Hardware.StandardMinMemoryGiB},
		[]any{cfg.Hardware.HeavyMinMemoryGiB, cfg.Hardware.StandardMinMemoryGiB}, "updated hardware tier thresholds")
	r1, r2 := old.Routing, cfg.Routing
	changed([]any{r1.PrefixAffinity, r1.BlockChars, r1.MaxBlocks, r1.AffinityMaxImbalance, r1.LargeRequestTokens, r1.CharsPerToken},
		[]any{r2.PrefixAffinity, r2.BlockChars, r2.MaxBlocks, r2.AffinityMaxImbalance, r2.LargeRequestTokens, r2.CharsPerToken},
		"updated routing settings")

	// These are wired up once at startup.
	restart := func(a, b any, what string) { changed(a, b, what+" changed: takes effect after a restart") }
	restart(old.Listen, cfg.Listen, "listen")
	restart(old.TLS.Enabled(), cfg.TLS.Enabled(), "tls on/off")
	restart([]any{old.Health.Disabled, old.Health.Path, old.Health.Interval, old.Health.Timeout},
		[]any{cfg.Health.Disabled, cfg.Health.Path, cfg.Health.Interval, cfg.Health.Timeout}, "health check schedule")
	restart([]any{old.Hardware.PollInterval, old.Hardware.Timeout}, []any{cfg.Hardware.PollInterval, cfg.Hardware.Timeout}, "hardware polling schedule")
	restart(old.Upstream, cfg.Upstream, "upstream timeouts")
	restart(old.Cluster, cfg.Cluster, "cluster settings")
	restart(old.Tracing, cfg.Tracing, "tracing settings")
	restart(old.StateDir, cfg.StateDir, "state_dir")
	changed(old.Logging, cfg.Logging, "updated logging settings")
	restart([]any{r1.AffinityTTL, r1.AffinityCapacity}, []any{r2.AffinityTTL, r2.AffinityCapacity}, "prefix table size")
	return out
}
