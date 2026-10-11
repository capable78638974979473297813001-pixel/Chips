# Speed and customization

## How SiliconRouter keeps requests fast

| Feature | What it prevents | Setting |
|---|---|---|
| **First-token failover** | A backend that accepts a request and then stalls (hung GPU, wedged engine) holding a user forever | `retry.first_token_timeout` (default 60s) |
| **Latency-aware routing** | Traffic going to a degraded node: thermally throttled, on a shared GPU, or simply slower | automatic |
| **Response cache** | Recomputing an answer that can't change | `cache.enabled` |
| **Request coalescing** | Many identical requests arriving together each using a GPU | automatic with the cache |
| **Exact token counts near limits** | A prompt sent to a backend too small for it, or a large backend left unused | backend `tokenize` |
| **Priority classes** | Batch jobs filling the queue ahead of interactive users | `auth.keys[].priority`, `queue.priority_header` |
| **Load shedding** | Interactive users turned away because batch work filled the queue | automatic with priorities |
| **Model fallbacks** | Errors or long waits while a model's GPUs are down or saturated | `models.fallbacks` |
| **Prefix-cache routing** | Re-reading a whole conversation on every turn | `routing.prefix_affinity` (default on) |
| **Fair queue + backpressure** | Overloading a GPU until every request on it crawls | `max_concurrency`, `queue.*` |
| **Hardware tiers** | Small requests waiting behind big ones on the big GPUs | `tier` / `tier: auto` |
| **Engine-aware routing** | Sending work to an engine whose own queue is backing up or whose KV cache is full, including from load the router can't see | backend `metrics_path` |

### First-token failover

For streaming requests, the router waits for the first bytes of the reply before it sends anything
to the client. If a backend produces nothing within `first_token_timeout`, the router cancels the
request there, counts it against that backend, and sends it to another one. The user sees a
slightly late first token, not a hang. The last attempt (`retry.max_attempts`) always waits, since
a slow answer beats none.

A non-streaming response arrives all at once when generation ends, so a timeout can't tell
"stalled" from "writing a long answer". Set `upstream.response_header_timeout` to put a hard limit
there, longer than your longest legitimate generation (for example `300s`). Without it, a hung
engine holds non-streaming requests for as long as clients wait. This happened in the
[real-hardware tests](VALIDATION.md), when two engines sharing one GPU stalled. With
`metrics_path`, a hung engine with requests piling up inside it is also marked saturated, so new
work goes elsewhere.

### Latency-aware routing

The router keeps a moving average of each backend's time to first token on streaming requests,
visible as `ttft_ms` in `/admin/backends` and as `siliconrouter_backend_ttft_seconds`. When two
backends in the same tier are equally loaded, the one at least 30% faster wins. Load always comes
first, so a fast node isn't flooded. A first-token timeout counts as a very slow sample.

### Response cache

Repeated requests are answered from memory in well under a millisecond. Only answers that can't
change are cached by default:

- **embeddings**: any path containing `embed`, such as `/v1/embeddings` or Ollama's `/api/embed`;
- **generations with temperature 0** (greedy decoding), in any API's spelling: `temperature`,
  `options.temperature`, `parameters.temperature`, `generationConfig.temperature`.

`mode: all` caches every successful response, including sampled ones; use it only where returning
the same answer to the same question is what you want. Streams are cached whole and replayed. Only
`200` responses are stored. A cached response carries `X-SiliconRouter-Cache: hit`; a client can skip
the cache with `Cache-Control: no-cache`.

**Coalescing.** When identical cacheable requests arrive while the first is still being computed,
they wait for its answer instead of each going to a GPU, so a burst of 50 identical requests costs
one generation. If that answer turns out not to be cacheable (an error, say), each waiting request
then computes its own.

By default each API key has its own cache, so one tenant never receives another's answers. Set
`share_across_keys: true` when every key belongs to the same team. The cache stores hashes of
requests, never the prompts themselves, but it does hold responses in memory.

### Engine-aware routing

```yaml
backends:
  - {name: h100-1, url: "http://10.0.0.11:8000", metrics_path: /metrics}   # vLLM, SGLang, TGI
  - {name: mac-1, url: "http://10.0.0.20:8080", metrics_path: /metrics}    # llama.cpp --metrics
engine_metrics:
  interval: 2s
  timeout: 1s
  kv_saturated: 0.95    # KV-cache usage at which an engine counts as full
```

The router counts its own requests, but it can't see:
- traffic from other routers or from clients that call an engine directly;
- how full each engine's KV cache is. GPU memory from chipagent doesn't show it, because engines
  reserve their memory up front.

With `metrics_path`, the router scrapes each engine's Prometheus metrics: requests running and
waiting, and KV-cache usage. It understands the metric names of:
- vLLM, `vllm:num_requests_*` and `vllm:kv_cache_usage_perc` (or `gpu_cache_usage_perc` on v0);
- SGLang, `sglang:num_running_reqs`, `num_queue_reqs` and `token_usage`;
- TGI, `tgi_queue_size` and `tgi_batch_current_size`;
- llama.cpp started with `--metrics`, `llamacpp:requests_*` and `kv_cache_usage_ratio`;
- Triton, `nv_inference_pending_request_count`.

The readings affect routing in three ways:
- **Saturated engines go last.** An engine that is queueing requests internally, or whose KV cache
  is at `kv_saturated` or above, gets new work only when no other suitable backend has room. Extra
  work there would only wait inside the engine, or evict other conversations' cache.
- **The busiest view wins.** Within a tier, a backend's load is the higher of the router's own slot
  use and what its engine reports (requests running and waiting against `max_concurrency`, and KV
  usage).
- **A saturated prefix-cache holder is passed over** for a backend with room.

Readings older than three intervals are ignored, so a failing scrape never pins traffic.
`/admin/backends` shows each engine's state, and three gauges report it:
`siliconrouter_backend_engine_waiting`, `..._kv_cache_usage` and `..._saturated`. Engines without
these metrics (Ollama, most custom servers) are routed on the router's own counts, as before.

### Prompt sizes and context limits

Routing needs each prompt's size to skip backends whose `max_context_tokens` it would exceed. The
router estimates size without a tokenizer, by script:
- English and code: about `chars_per_token` (default 4) characters per token;
- Chinese, Japanese and Korean: about one token per character;
- other scripts: about 2.5 characters per token.

Far from every limit, a rough estimate is enough. When a request's estimate is within
`routing.exact_tokens_margin` (default 20%) of some backend's limit, the router asks the engine for
the exact count first. The backend needs `tokenize` set:

```yaml
backends:
  - {name: gpu-small, url: http://10.0.0.21:8000, max_context_tokens: 8192, tokenize: vllm}
  - {name: gpu-large, url: http://10.0.0.22:8000, max_context_tokens: 131072, tokenize: vllm}
```

`tokenize` can be `vllm` (counts chat requests with the model's chat template), `llamacpp`, or
`tgi`. The call is skipped for prompts far from any limit. If it fails, the router keeps its
estimate.

### Router overhead

`go test ./internal/server -bench .` measures the router against a backend that answers instantly,
with an 8 KB multi-turn chat request:

| | per request |
|---|---|
| Client → backend directly | ~26 µs |
| Client → router → backend | ~95–105 µs (two network hops instead of one) |
| Cache hit | ~45–55 µs (no backend) |

Real model calls take tens of milliseconds to minutes, so router overhead is well under 1% of a
request. Request bodies are scanned once, without being decoded, to pull out the model and prompt.
The scanner is fuzz-tested against Go's JSON decoder.

## Customization

### Model aliases

```yaml
models:
  aliases:
    gpt-4o: llama-3.1-70b
    text-embedding-3-small: bge-large-en
```

A client asking for `gpt-4o` is served by `llama-3.1-70b`, and the backend receives the real name.
Aliases work for model names in the JSON body and in URLs (Triton, KServe, Gemini, Azure).
`GET /v1/models` lists each alias whose target is being served, so OpenAI SDKs that check the
model list keep working.

### Model fallbacks

```yaml
models:
  fallbacks:
    llama-3.1-70b: [llama-3.1-8b]          # tried in order
    qwen2.5-72b: [qwen2.5-32b, qwen2.5-7b]
```

When a request's model can't take it, the router tries the next model in its list. That happens
when:
- no healthy backend serves the model;
- every backend for it failed;
- its queue is full, or the request was shed;
- the request waited `queue.max_wait` without getting a slot.

The fallback gets a fresh retry budget and a fresh queue wait. The backend receives the fallback's
name, and the response carries `X-SiliconRouter-Fallback: llama-3.1-8b`, so clients can tell. A
fallback's answer is never stored in the response cache under the original request.
`siliconrouter_model_fallbacks_total{model,fallback}` counts how often each fallback is used, and
request logs show it. Keys are real model names: put the alias's target, not the alias.

Fallbacks respect key policy: a key is never moved to a model its `models` list doesn't allow (see
OPERATIONS.md).

### Priority classes

```yaml
auth:
  keys:
    - {name: chat-app, key_env: CHAT_APP_KEY, priority: high}
    - {name: analytics, key_env: ANALYTICS_KEY}            # normal
    - {name: nightly-batch, key_env: BATCH_KEY, priority: low}
queue:
  priority_header: false   # true: clients may send X-SiliconRouter-Priority: high|normal|low
```

When every suitable GPU is busy, waiting requests are served high first, then normal, then low, and
in arrival order within each class. Priorities only matter under load; with free capacity, every
request is dispatched immediately. Request logs include the key's `client` name and `priority`.
Leave `priority_header` off unless every client is trusted, since it lets a client raise its own
priority.

**Load shedding.** When the queue is full (`queue.max_waiting`) and a request arrives with a higher
priority than the lowest one waiting, the newest lowest-priority waiter gives up its place. It gets
`429` with `Retry-After` (counted as `siliconrouter_rejected_total{reason="shed"}`), and the new
request queues instead. Under overload, interactive traffic displaces batch work instead of being
turned away. Requests of equal priority never displace each other.

### Per-backend settings

| Setting | Use |
|---|---|
| `tier` | `heavy`, `standard`, `light`, `reserve`, or `auto` with chipagent |
| `max_concurrency` | requests the backend runs at once; the rest queue in the router |
| `max_context_tokens` | skip this backend for prompts that won't fit |
| `models` | pin the model list instead of discovering it |
| `health_path` | the engine's own health endpoint |
| `api_key_env`, `api_key_header` | the backend's credentials, and the header they go in |
| `agent_url` | chipagent on the machine, for hardware tiers and telemetry |

Everything else is in [`examples/siliconrouter.yaml`](../examples/siliconrouter.yaml), with defaults.
