package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
)

// loadTimeout bounds loading one adapter: reading its weights from disk
// or a model hub.
const loadTimeout = 2 * time.Minute

// adapterLoads makes concurrent requests for the same adapter on the same
// backend share one load.
type adapterLoads struct {
	mu       sync.Mutex
	inflight map[string]*adapterLoad
}

type adapterLoad struct {
	done chan struct{}
	err  error
}

// setAdapter marks req as a request for a LoRA adapter when its model is one.
func setAdapter(req *pool.Request, models config.Models) (config.Adapter, bool) {
	a, ok := models.Adapter(req.Model)
	req.Base = ""
	if ok {
		req.Base = a.Base
	}
	return a, ok
}

// loadAdapter has b load adapter, through vLLM's runtime LoRA API
// (POST /v1/load_lora_adapter, enabled by VLLM_ALLOW_RUNTIME_LORA_UPDATING).
// An adapter that is already loaded counts as loaded.
func (s *Server) loadAdapter(ctx context.Context, b *pool.Backend, a config.Adapter) error {
	if a.Path == "" {
		return fmt.Errorf("models.adapters entry %q has no path to load it from", a.Name)
	}
	key := b.Name + "\x00" + a.Name
	s.loads.mu.Lock()
	if s.loads.inflight == nil {
		s.loads.inflight = map[string]*adapterLoad{}
	}
	if l, ok := s.loads.inflight[key]; ok {
		s.loads.mu.Unlock()
		select {
		case <-l.done:
			return l.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	l := &adapterLoad{done: make(chan struct{})}
	s.loads.inflight[key] = l
	s.loads.mu.Unlock()

	// Not tied to this request: others may be waiting on the same load.
	lctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	l.err = s.postLoad(lctx, b, a)
	s.loads.mu.Lock()
	delete(s.loads.inflight, key)
	s.loads.mu.Unlock()
	close(l.done)
	if l.err == nil {
		s.pool.NoteAdapter(b, a.Name)
		s.log.Info("loaded LoRA adapter", "backend", b.Name, "adapter", a.Name, "base", a.Base)
	} else {
		s.log.Warn("loading LoRA adapter failed", "backend", b.Name, "adapter", a.Name, "error", l.err)
	}
	return l.err
}

func (s *Server) postLoad(ctx context.Context, b *pool.Backend, a config.Adapter) error {
	body, _ := json.Marshal(map[string]string{"lora_name": a.Name, "lora_path": a.Path})
	u := *b.URL
	u.Path = strings.TrimSuffix(u.Path, "/") + "/v1/load_lora_adapter"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	b.SetAuth(req.Header)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(string(msg)), "already"):
		return nil // "The lora adapter ... has already been loaded."
	}
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
}
