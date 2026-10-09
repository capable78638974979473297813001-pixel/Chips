// Command mockllm is a fake OpenAI-compatible inference server for demos and
// load tests. It imitates the costs that matter for routing: a limited number
// of concurrent slots, prefill time proportional to the *uncached* prompt,
// and a prefix cache that makes repeat conversations faster.
package main

import (
	"container/list"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/prompt"
)

const blockChars = 256 // must match the cache granularity being simulated

type server struct {
	name       string
	model      string
	prefillTPS float64
	decodeTPS  float64
	failRate   float64
	slots      chan struct{}

	mu       sync.Mutex
	cache    map[uint64]*list.Element
	lru      *list.List
	capacity int
}

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	name := flag.String("name", "mock", "name reported in responses")
	model := flag.String("model", "llama-3-8b", "model ID served")
	prefill := flag.Float64("prefill-tps", 4000, "prompt tokens processed per second when not cached")
	decode := flag.Float64("decode-tps", 60, "generated tokens per second per request")
	slots := flag.Int("max-concurrency", 8, "requests processed at once; the rest queue (like a full GPU)")
	cacheBlocks := flag.Int("cache-blocks", 20000, "prefix cache capacity in blocks of 256 chars")
	failRate := flag.Float64("fail-rate", 0, "fraction of requests answered with 503")
	flag.Parse()

	s := &server{
		name: *name, model: *model, prefillTPS: *prefill, decodeTPS: *decode, failRate: *failRate,
		slots: make(chan struct{}, *slots), cache: map[uint64]*list.Element{}, lru: list.New(), capacity: *cacheBlocks,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []map[string]string{{"id": s.model, "object": "model"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/completions", s.handleChat)
	log.Printf("mockllm %q serving %s on %s", s.name, s.model, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := prompt.Parse(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if info.Model != "" && info.Model != s.model {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"model %q not found"}}`, info.Model), http.StatusNotFound)
		return
	}
	if s.failRate > 0 && rand.Float64() < s.failRate {
		http.Error(w, "simulated failure", http.StatusServiceUnavailable)
		return
	}

	select {
	case s.slots <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.slots }()

	hashes := prompt.BlockHashes(info.Text, blockChars, 1<<20)
	cachedBlocks := s.lookup(hashes)
	promptTokens := prompt.EstimateTokens(info.Text, 4)
	cachedTokens := min(cachedBlocks*blockChars/4, promptTokens)
	if !sleep(r, time.Duration(float64(promptTokens-cachedTokens)/s.prefillTPS*float64(time.Second))) {
		return
	}

	n := info.MaxTokens
	if n <= 0 {
		n = 32
	}
	words := make([]string, n)
	for i := range words {
		words[i] = fmt.Sprintf("tok%d", i)
	}
	reply := fmt.Sprintf("[%s] %s", s.name, strings.Join(words, " "))
	// Engines keep the KV of generated tokens too, so the next turn of this
	// conversation (prompt + this reply) is a cache hit on this server.
	s.insert(hashes)
	s.insert(prompt.BlockHashes(info.Text+"\x1eassistant\x1f"+reply, blockChars, 1<<20))

	usage := map[string]any{
		"prompt_tokens": promptTokens, "completion_tokens": n, "total_tokens": promptTokens + n,
		"prompt_tokens_details": map[string]int{"cached_tokens": cachedTokens},
	}
	id := fmt.Sprintf("chatcmpl-%x", rand.Uint64())
	perToken := time.Duration(float64(time.Second) / s.decodeTPS)
	w.Header().Set("X-Mock-Server", s.name)

	if !info.Stream {
		if !sleep(r, time.Duration(n)*perToken) {
			return
		}
		writeJSON(w, map[string]any{
			"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": s.model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "length",
				"message": map[string]string{"role": "assistant", "content": reply}}},
			"usage": usage,
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	tokens := strings.SplitAfter(reply, " ")
	for i, t := range tokens {
		if i > 0 && !sleep(r, perToken) {
			return
		}
		send(map[string]any{"id": id, "object": "chat.completion.chunk", "model": s.model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]string{"content": t}}}})
	}
	send(map[string]any{"id": id, "object": "chat.completion.chunk", "model": s.model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]string{}, "finish_reason": "length"}}, "usage": usage})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// lookup returns how many leading blocks are cached.
func (s *server) lookup(hashes []uint64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, h := range hashes {
		el, ok := s.cache[h]
		if !ok {
			break
		}
		s.lru.MoveToFront(el)
		n++
	}
	return n
}

func (s *server) insert(hashes []uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range hashes {
		if el, ok := s.cache[h]; ok {
			s.lru.MoveToFront(el)
			continue
		}
		s.cache[h] = s.lru.PushFront(h)
		for s.lru.Len() > s.capacity {
			old := s.lru.Back()
			s.lru.Remove(old)
			delete(s.cache, old.Value.(uint64))
		}
	}
}

func sleep(r *http.Request, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
