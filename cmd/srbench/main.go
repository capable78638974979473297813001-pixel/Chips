// Command srbench drives multi-turn chat traffic at an OpenAI-compatible
// endpoint and reports latency and prefix-cache reuse. Point it at
// SiliconRouter, then at a plain round-robin balancer, and compare.
package main

import (
	"bufio"
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
	ttft         time.Duration // streaming only: time to the first content
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
	models := flag.String("models", "", "comma-separated models (e.g. LoRA adapters) given to conversations in turn; overrides -model")
	stream := flag.Bool("stream", false, "stream responses and report time to first token")
	timeout := flag.Duration("timeout", 5*time.Minute, "per-request timeout")
	flag.Parse()

	names := []string{*model}
	if *models != "" {
		names = strings.Split(*models, ",")
	}
	system := filler("You are a helpful assistant for ACME Corp. ", *systemChars)
	client := &http.Client{Timeout: *timeout}
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
				res, reply := send(client, *url, *apiKey, names[c%len(names)], history, *maxTokens, *stream)
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

func send(client *http.Client, url, apiKey, model string, history []message, maxTokens int, stream bool) (result, string) {
	req := map[string]any{"model": model, "messages": history, "max_tokens": maxTokens}
	if stream {
		req["stream"] = true
		req["stream_options"] = map[string]any{"include_usage": true}
	}
	body, _ := json.Marshal(req)
	hreq, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	hreq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	start := time.Now()
	resp, err := client.Do(hreq)
	if err != nil {
		return result{err: err, latency: time.Since(start)}, ""
	}
	defer resp.Body.Close()
	res := result{status: resp.StatusCode, backend: resp.Header.Get("X-SiliconRouter-Backend")}
	var raw []byte
	if stream && resp.StatusCode == http.StatusOK {
		raw, res.ttft, err = readEvents(resp.Body, start)
	} else {
		raw, err = io.ReadAll(resp.Body)
	}
	res.latency = time.Since(start)
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
	if stream {
		return readStream(res, start, raw)
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

// readEvents reads a server-sent event stream to its end, noting when the
// first content arrived.
func readEvents(body io.Reader, start time.Time) ([]byte, time.Duration, error) {
	var raw bytes.Buffer
	var ttft time.Duration
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if ttft == 0 && bytes.Contains(line, []byte(`"content":"`)) && !bytes.Contains(line, []byte(`"content":""`)) {
			ttft = time.Since(start)
		}
		raw.Write(line)
		raw.WriteByte('\n')
	}
	return raw.Bytes(), ttft, sc.Err()
}

// readStream assembles a streamed reply and its usage.
func readStream(res result, start time.Time, raw []byte) (result, string) {
	var reply strings.Builder
	done := false
	for _, line := range strings.Split(string(raw), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var ev struct {
			Choices []struct {
				Delta message `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				PromptTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		for _, c := range ev.Choices {
			reply.WriteString(c.Delta.Content)
		}
		if ev.Usage != nil {
			res.promptTokens = ev.Usage.PromptTokens
			res.cachedTokens = ev.Usage.PromptTokensDetails.CachedTokens
		}
	}
	if !done {
		res.err = fmt.Errorf("stream ended without [DONE] after %s", time.Since(start).Round(time.Millisecond))
		return res, ""
	}
	return res, reply.String()
}

func report(results <-chan result, elapsed time.Duration) {
	var lat, ttfts []time.Duration
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
		if r.ttft > 0 {
			ttfts = append(ttfts, r.ttft)
		}
		promptTok += r.promptTokens
		cachedTok += r.cachedTokens
		perBackend[r.backend]++
	}
	pct := func(d []time.Duration, p float64) time.Duration {
		if len(d) == 0 {
			return 0
		}
		return d[min(len(d)-1, int(p*float64(len(d))))].Round(time.Millisecond)
	}
	slices.Sort(lat)
	slices.Sort(ttfts)
	fmt.Printf("requests      %d ok, %d failed in %s (%.1f req/s)\n", ok, failed, elapsed.Round(time.Millisecond), float64(ok)/elapsed.Seconds())
	fmt.Printf("latency       p50 %s  p95 %s  p99 %s\n", pct(lat, .5), pct(lat, .95), pct(lat, .99))
	if len(ttfts) > 0 {
		fmt.Printf("first token   p50 %s  p95 %s  p99 %s\n", pct(ttfts, .5), pct(ttfts, .95), pct(ttfts, .99))
	}
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
