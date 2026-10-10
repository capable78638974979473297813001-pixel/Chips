# Speed and customization

## How SiliconRouter keeps requests fast

| Feature | What it prevents | Setting |
|---|---|---|
| **First-token failover** | A backend that accepts a request and then stalls (hung GPU, wedged engine) holding a user forever | `retry.first_token_timeout` (default 60s) |
| **Latency-aware routing** | Traffic going to a degraded node: thermally throttled, on a shared GPU, or simply slower | automatic |
| **Response cache** | Recomputing an answer that can't change | `cache.enabled` |
| **Priority classes** | Batch jobs filling the queue ahead of interactive users | `auth.keys[].priority`, `queue.priority_header` |
| **Prefix-cache routing** | Re-reading a whole conversation on every turn | `routing.prefix_affinity` (default on) |
| **Fair queue + backpressure** | Overloading a GPU until every request on it crawls | `max_concurrency`, `queue.*` |
| **Hardware tiers** | Small requests waiting behind big ones on the big GPUs | `tier` / `tier: auto` |

### First-token failover

For streaming requests, the router waits for the first bytes of the reply before it sends anything
to the client. If a backend produces nothing within `first_token_timeout`, the router cancels the
request there, counts it against that backend, and sends it to another one. The user sees a
slightly late first token, not a hang. The last attempt (`retry.max_attempts`) always waits, since
a slow answer beats none.

A non-streaming response arrives all at once when generation ends, so a timeout can't tell
"stalled" from "writing a long answer". Use `upstream.response_header_timeout` if you want a hard
limit there.

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

By default each API key has its own cache, so one tenant never receives another's answers. Set
`share_across_keys: true` when every key belongs to the same team. The cache stores hashes of
requests, never the prompts themselves, but it does hold responses in memory.

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
