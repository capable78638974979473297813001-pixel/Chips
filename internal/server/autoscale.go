package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
)

// scaleUps rate-limits the scale-up webhook per model.
type scaleUps struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// prepareHold decides whether req may wait for a backend to start, and if
// it would (no backend for its model is up), stretches the queue wait to
// autoscaling.cold_start_wait and asks for a backend. A request with
// fallbacks left does not wait: a smaller model that is up beats a long wait.
func (s *Server) prepareHold(st *state, req *pool.Request, wait *queueWait, fallbacksLeft int, rl *requestLog) {
	as := st.cfg.Autoscaling
	req.Hold = as.ColdStartWait > 0 && fallbacksLeft == 0
	if as.ColdStartWait == 0 || !s.pool.Cold(*req) {
		return
	}
	s.scaleUp(st, req.Model, req.Base)
	if req.Hold {
		rl.coldStart = true
		wait.max = as.ColdStartWait.D()
		wait.reset()
	}
}

// scaleUp calls the scale-up webhook for model, at most once per
// webhook.interval, without making the request wait for it.
func (s *Server) scaleUp(st *state, model, base string) {
	if base != "" {
		model = base // deployments serve the base model
	}
	s.coldStarts.Inc(model)
	wh := st.cfg.Autoscaling.Webhook
	if wh.URL == "" {
		return
	}
	s.scaled.mu.Lock()
	if s.scaled.last == nil {
		s.scaled.last = map[string]time.Time{}
	}
	if time.Since(s.scaled.last[model]) < wh.Interval.D() {
		s.scaled.mu.Unlock()
		return
	}
	s.scaled.last[model] = time.Now()
	s.scaled.mu.Unlock()

	var held int
	for _, d := range s.pool.Demand(st.cfg.Autoscaling.TargetUtilization, st.cfg.Autoscaling.BackendCapacity) {
		if d.Model == model {
			held = d.Held
		}
	}
	body, _ := json.Marshal(map[string]any{
		"event": "scale_up", "model": model, "reason": "no backend up for this model",
		"held_requests": held + 1, "router": st.cfg.Cluster.NodeName, "time": time.Now().UTC().Format(time.RFC3339),
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(body))
		if err != nil {
			s.log.Warn("scale-up webhook failed", "model", model, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if wh.Secret != "" {
			req.Header.Set("Authorization", "Bearer "+wh.Secret)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			s.log.Warn("scale-up webhook failed", "model", model, "error", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			s.log.Warn("scale-up webhook failed", "model", model, "status", resp.Status)
			return
		}
		s.log.Info("asked for a backend to start", "model", model, "webhook", wh.URL)
	}()
}

func (s *Server) demand() []pool.ModelDemand {
	as := s.conf().Autoscaling
	return s.pool.Demand(as.TargetUtilization, as.BackendCapacity)
}

// handleAutoscale serves demand per model for autoscalers: GET /autoscale
// lists every model, GET /autoscale/{model} one (zeros for a model with no
// demand), e.g. for KEDA's metrics-api scaler with valueLocation
// desired_backends.
func (s *Server) handleAutoscale(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	all := s.demand()
	model := r.PathValue("model")
	if model == "" {
		json.NewEncoder(w).Encode(map[string]any{"models": all})
		return
	}
	for _, d := range all {
		if d.Model == model {
			json.NewEncoder(w).Encode(d)
			return
		}
	}
	json.NewEncoder(w).Encode(pool.ModelDemand{Model: model})
}

func (s *Server) registerDemandMetrics() {
	gauge := func(name, help string, value func(pool.ModelDemand) int) {
		s.reg.NewGaugeFunc(name, help, []string{"model"}, func(emit func(float64, ...string)) {
			for _, d := range s.demand() {
				emit(float64(value(d)), d.Model)
			}
		})
	}
	gauge("siliconrouter_model_inflight", "Requests in flight per model (adapters count toward their base model).",
		func(d pool.ModelDemand) int { return d.Inflight })
	gauge("siliconrouter_model_queued", "Requests waiting for a backend slot per model, including held ones.",
		func(d pool.ModelDemand) int { return d.Queued })
	gauge("siliconrouter_model_held", "Requests waiting because no backend for the model is up (cold start).",
		func(d pool.ModelDemand) int { return d.Held })
	gauge("siliconrouter_model_ready_backends", "Backends in rotation that serve the model.",
		func(d pool.ModelDemand) int { return d.ReadyBackends })
	gauge("siliconrouter_model_desired_backends", "Backends that would serve the model's current demand at autoscaling.target_utilization.",
		func(d pool.ModelDemand) int { return d.DesiredBackends })
}
