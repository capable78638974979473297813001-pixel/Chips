// Command srbench drives multi-turn chat traffic at an OpenAI-compatible
// endpoint and reports latency and prefix-cache reuse. Point it at
// SiliconRouter, then at a plain round-robin balancer, and compare.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type result struct {
	latency      time.Duration
	status       int
	backend      string
	promptTokens int
	cachedTokens int
	err          error
}

func main() {
	url := flag.String("url", "http://localhost:8080/v1/chat/completions", "chat completions endpoint")
	model := flag.String("model", "llama-3-8b", "model name")
	apiKey := flag.String("api-key", os.Getenv("SILICONROUTER_API_KEY"), "bearer token, if the endpoint needs one")
	conversations := flag.Int("conversations", 40, "number of independent conversations")
	turns := flag.Int("turns", 6, "user turns per conversation")
	concurrency := flag.Int("concurrency", 16, "conversations in flight at once")
	systemChars := flag.Int("system-chars", 3000, "length of the shared system prompt")
	userChars := flag.Int("user-chars", 600, "length of each user message")
	maxTokens := flag.Int("max-tokens", 48, "completion tokens per turn")
	flag.Parse()

	system := filler("You are a helpful assistant for ACME Corp. ", *systemChars)
	client := &http.Client{Timeout: 5 * time.Minute}
	results := make(chan result, *conversations**turns)
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup
	start := time.Now()

	for c := range *conversations {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			history := []message{{Role: "system", Content: system}}
			for t := range *turns {
				q := fmt.Sprintf("Conversation %d, question %d (%x): ", c, t, rand.Uint32())
				history = append(history, message{Role: "user", Content: filler(q, *userChars)})
				res, reply := send(client, *url, *apiKey, *model, history, *maxTokens)
				results <- res
				if res.err != nil {
					return
				}
				history = append(history, message{Role: "assistant", Content: reply})
			}
		}()
	}
	wg.Wait()
	close(results)
	report(results, time.Since(start))
}

func send(client *http.Client, url, apiKey, model string, history []message, maxTokens int) (result, string) {
	body, _ := json.Marshal(map[string]any{"model": model, "messages": history, "max_tokens": maxTokens})
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return result{err: err, latency: time.Since(start)}, ""
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	res := result{latency: time.Since(start), status: resp.StatusCode, backend: resp.Header.Get("X-SiliconRouter-Backend")}
	if res.backend == "" {
		res.backend = resp.Header.Get("X-Mock-Server")
	}
	if err != nil {
		res.err = err
		return res, ""
	}
	if resp.StatusCode != http.StatusOK {
		res.err = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
		return res, ""
	}
	var out struct {
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		res.err = fmt.Errorf("unexpected response: %.200s", raw)
		return res, ""
	}
	res.promptTokens = out.Usage.PromptTokens
	res.cachedTokens = out.Usage.PromptTokensDetails.CachedTokens
	return res, out.Choices[0].Message.Content
}

func report(results <-chan result, elapsed time.Duration) {
	var lat []time.Duration
	var ok, failed, promptTok, cachedTok int
	perBackend := map[string]int{}
	var firstErr error
	for r := range results {
		if r.err != nil {
			failed++
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		ok++
		lat = append(lat, r.latency)
		promptTok += r.promptTokens
		cachedTok += r.cachedTokens
		perBackend[r.backend]++
	}
	slices.Sort(lat)
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[min(len(lat)-1, int(p*float64(len(lat))))]
	}
	fmt.Printf("requests      %d ok, %d failed in %s (%.1f req/s)\n", ok, failed, elapsed.Round(time.Millisecond), float64(ok)/elapsed.Seconds())
	fmt.Printf("latency       p50 %s  p95 %s  p99 %s\n", pct(.5).Round(time.Millisecond), pct(.95).Round(time.Millisecond), pct(.99).Round(time.Millisecond))
	if promptTok > 0 {
		fmt.Printf("prefix cache  %.1f%% of prompt tokens reused (%d of %d)\n", 100*float64(cachedTok)/float64(promptTok), cachedTok, promptTok)
	}
	names := make([]string, 0, len(perBackend))
	for n := range perBackend {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		label := n
		if label == "" {
			label = "(unknown)"
		}
		fmt.Printf("backend       %-20s %d requests\n", label, perBackend[n])
	}
	if firstErr != nil {
		log.Printf("first error: %v", firstErr)
	}
}

func filler(prefix string, n int) string {
	const words = "the quick brown fox jumps over a lazy dog while engineers tune gpu clusters for low latency inference "
	var sb strings.Builder
	sb.WriteString(prefix)
	for sb.Len() < n {
		sb.WriteString(words)
	}
	return sb.String()[:max(n, len(prefix))]
}
