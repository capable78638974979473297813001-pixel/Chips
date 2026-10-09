# SiliconRouter

**A traffic controller for self-hosted AI.** SiliconRouter sits in front of your GPU servers and
decides which one handles each request, so a mixed pile of hardware behaves like one fast, reliable
endpoint.

```
                    your apps (anything using the OpenAI API)
                                     │
                          ┌──────────▼──────────┐
                          │    SiliconRouter     │  one URL, one API key
                          │  queue · cache-aware │
                          │  routing · failover  │
                          └──┬───────┬────────┬──┘
                             │       │        │
                     NVIDIA H100  Mac Studio  AMD MI300
                       (heavy)    (standard)  (reserve)
                       vLLM        Ollama     vLLM/ROCm
```

## What it does, simply

1. **Organizes the line.** Each GPU server gets only as many requests as it can handle well. Extra
   requests wait in one fair queue instead of piling onto a server until it crawls. When the queue
   is full, clients get a clean `429 Retry-After` instead of a timeout.
2. **Remembers the past.** Inference servers keep a "KV cache" of text they've already processed.
   SiliconRouter sends each follow-up message in a conversation back to the server that already
   holds the earlier messages, so that server skips re-reading them. Sending the request anywhere
   else would mean recomputing the whole history.
3. **Mixes and matches hardware.** You tag each server as `heavy`, `standard`, `light` or `reserve`.
   Big prompts go to heavy GPUs, small ones to cheaper machines, and reserve hardware only gets
   traffic when everything else is full or down. Servers too small for a prompt's context are skipped.
4. **Survives failures.** If a server dies mid-request, the request is retried on another server
   before the user sees anything. Health checks take dead servers out of rotation and bring them
   back when they recover.

Your apps don't change: point any OpenAI client at SiliconRouter instead of a single server.

## Quick start

```bash
make demo        # 3 simulated GPU servers + the router on :8080 (no GPU needed)
make bench       # in a second terminal: multi-turn chat load test
```

Or with Docker:

```bash
docker compose -f deploy/docker-compose.yaml up --build
```

Then talk to it like any OpenAI endpoint:

```bash
curl localhost:8080/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"llama-3-8b","messages":[{"role":"user","content":"Hello"}]}' -i
# X-SiliconRouter-Backend: h100-node-1
# X-SiliconRouter-Prefix-Cache: miss
```

For real hardware, copy [`examples/siliconrouter.yaml`](examples/siliconrouter.yaml), list your
servers, and run:

```bash
go build -o bin/ ./cmd/...
bin/siliconrouter -config siliconrouter.yaml
```

Works with anything that speaks the OpenAI API: **vLLM, SGLang, llama.cpp server, Ollama, TGI,
LM Studio**. Enable prefix caching on the engine (vLLM: `--enable-prefix-caching`, on by default in
recent versions) to get the cache benefit.

## Measured results (simulator)

These numbers come from `srbench` running against `mockllm`, a simulated GPU server whose prefill
time scales with the uncached part of the prompt. The setup is three nodes, with 30 conversations
of 10 turns each, 8 conversations running at once. The simulator models cache and queueing
behavior, not real GPU kernels, so **measure on your own hardware before quoting numbers.**

| Routing                               | p95 latency | p99 latency |
|---------------------------------------|-------------|-------------|
| Least-loaded (`prefix_affinity: false`) | 878 ms      | 1371 ms     |
| SiliconRouter cache-aware              | 603 ms      | 607 ms      |

With cache-aware routing, 96% of requests (288 of 300) went to a server that already held their
prompt prefix. When a GPU node was killed in the middle of a run, all 320 requests still succeeded:
8 in-flight requests were retried on other nodes, and the dead node was removed automatically.

Run `srbench` against your own cluster to get real numbers. It reports latency percentiles and how
many prompt tokens your servers served from cache (using the `cached_tokens` field that vLLM and
OpenAI-compatible servers return).

## API

| Endpoint                  | Purpose                                                       |
|---------------------------|---------------------------------------------------------------|
| `POST /v1/*`              | Chat completions, completions, embeddings: routed to a backend |
| `GET /v1/models`          | Models served by healthy backends                             |
| `GET /admin/backends`     | Live state: health, in-flight requests, models, last error    |
| `GET /metrics`            | Prometheus metrics                                            |
| `GET /healthz`, `/readyz` | Liveness, and readiness (at least one healthy backend)        |

Request headers:

- `Authorization: Bearer <key>`: required when `auth` keys are configured. It is never forwarded
  upstream; each backend gets its own `api_key_env` token instead.
- `X-Session-ID: <id>` (optional): pins a conversation to one backend even if the client trims
  old messages.

Response headers: `X-SiliconRouter-Backend` (which server answered) and
`X-SiliconRouter-Prefix-Cache` (`hit` when routed to the server holding the conversation).

Key metrics: `siliconrouter_prefix_cache_routing_total{result="hit|rebalanced|miss"}`,
`siliconrouter_queue_depth`, `siliconrouter_queue_wait_seconds`, `siliconrouter_backend_inflight`,
`siliconrouter_backend_healthy`, `siliconrouter_retries_total`, `siliconrouter_request_duration_seconds`.

## How routing decides

For each request, the router:

1. Parses the JSON body for the model, an approximate prompt size, and a chain of hashes over
   256-character blocks of the conversation. These are the same kind of prefix keys inference
   engines use for their own caches.
2. Filters to backends that are healthy, serve that model, have room for the context, and haven't
   already failed this request.
3. **Cache first:** if a backend recently served the longest matching prefix and has a free slot,
   it gets the request, unless it is much busier than the alternatives (`affinity_max_imbalance`).
4. **Then hardware fit:** large prompts try `heavy → standard → light → reserve`; small prompts try
   `light → standard → heavy → reserve`. Within a tier, the least utilized backend wins.
5. **Otherwise wait:** if every suitable backend is full, the request joins a FIFO queue. A request
   that can only fit on a big GPU doesn't block smaller requests that fit elsewhere.
6. **Retry:** on a connection error or a `429/502/503/504` before any bytes reach the client, the
   request is resent to a different backend (up to `retry.max_attempts`). Streams are relayed
   token-by-token with no buffering.

## Project layout

```
cmd/siliconrouter   the router
cmd/mockllm         simulated GPU inference server for demos and tests
cmd/srbench         multi-turn chat load generator
internal/server     HTTP proxy, retries, streaming, auth, metrics endpoints
internal/pool       backends, scheduler/queue, health checking
internal/affinity   prefix → backend LRU table
internal/prompt     request parsing, prefix hashing, size estimates
internal/config     YAML config, defaults, validation
```

`make test` runs the suite with the race detector. The router depends only on the Go standard
library and `gopkg.in/yaml.v3`.

## Status and roadmap

See [docs/ROADMAP.md](docs/ROADMAP.md) for what's built, what's next, and how the open-source and
commercial editions split.

## License

Apache 2.0. See [LICENSE](LICENSE).
