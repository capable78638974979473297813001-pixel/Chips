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
4. **Knows the hardware, whoever made it.** An optional C++ agent, `chipagent`, runs on each
   server (Linux on any CPU architecture, Windows, macOS). It identifies the CPU and every PCI GPU
   or AI accelerator: NVIDIA, AMD, Intel, Apple, Gaudi, Inferentia, TPU, Ascend, and unknown
   vendors. With `tier: auto` the router tiers each node from what it actually has, sees GPU load
   from outside work, and lists the whole mixed fleet in one inventory. See
   [docs/HARDWARE.md](docs/HARDWARE.md).
5. **Stays fast.** A streaming request whose backend stalls is moved to another backend before the
   user sees anything. Equally loaded nodes are chosen by how fast they have actually been
   answering. Repeated embeddings and temperature-0 requests can be answered from a response cache
   in under a millisecond. Interactive traffic can be given priority over batch jobs. See
   [docs/TUNING.md](docs/TUNING.md).
6. **Ready for a platform team.** Built-in HTTPS with certificate hot-swap and optional mutual TLS,
   separate admin keys, per-team limits (concurrency, requests per minute, tokens per day, allowed
   models, max tokens per request), and per-team usage for chargeback. Config changes apply live
   via `SIGHUP` or `POST /admin/reload`. Backends can be found in DNS (Kubernetes headless
   services, Consul SRV) and drained for maintenance without dropping a request. Several routers
   can run as one cluster, sharing conversation placement, quotas and limits. Traces go
   to any OpenTelemetry collector. A Helm chart, systemd units and release builds are included.
   See [docs/OPERATIONS.md](docs/OPERATIONS.md).
7. **Survives failures.** If a server dies mid-request, the request is retried on another server
   before the user sees anything. Health checks take dead servers out of rotation and bring them
   back when they recover. When a model has no capacity left, requests can fall back to a smaller
   model instead of failing, and under overload high-priority traffic displaces queued batch work.

Your apps don't change: point any OpenAI client at SiliconRouter instead of a single server.
Other AI APIs work too, including Anthropic-style, Ollama, TGI, Triton/KServe, Gemini-style, and any custom
HTTP service. The router forwards every path and reads the model from the body or URL when it can.
See [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md) for the full list of chips and AI software, and
its limits.

## Quick start

```bash
make demo        # 3 simulated GPU servers + the router on :8080 (no GPU needed)
make bench       # in a second terminal: multi-turn chat load test
make demo-mixed  # same, plus a simulated chipagent per node (NVIDIA, AMD, Apple) and tier: auto
```

`demo-mixed` also needs CMake and a C++17 compiler.

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

Works with **vLLM, SGLang, llama.cpp server, Ollama, TGI, LM Studio, Triton, KServe, TensorFlow
Serving**, and any other HTTP inference server. Enable prefix caching on the engine (vLLM: `--enable-prefix-caching`, on by default in
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
| any other path            | Forwarded to a backend: OpenAI `/v1/*`, `/v1/messages`, Ollama `/api/*`, TGI, Triton/KServe `/v2/*`, ... |
| `GET /v1/models`          | Models served by healthy backends                             |
| `GET /admin/backends`     | Live state: health, in-flight requests, models, hardware, last error |
| `GET /admin/usage`        | Requests, rejections and tokens per client (API key name)     |
| `POST /admin/reload`      | Re-read the config file and apply it live (also `SIGHUP`)      |
| `POST /admin/backends/{name}/drain`, `/undrain` | Take a backend out of rotation for maintenance, or put it back |
| `GET /metrics`            | Prometheus metrics                                            |
| `GET /healthz`, `/readyz` | Liveness, and readiness (at least one healthy backend)        |

Request headers:

- `Authorization: Bearer <key>`: required when `auth` keys are configured. It is never forwarded
  upstream; each backend gets its own `api_key_env` token instead.
- `X-Session-ID: <id>` (optional): pins a conversation to one backend even if the client trims
  old messages.

Response headers: `X-SiliconRouter-Backend` (which server answered),
`X-SiliconRouter-Prefix-Cache` (`hit` when routed to the server holding the conversation),
`X-SiliconRouter-Cache` (`hit`/`miss` when the response cache is enabled), and
`X-SiliconRouter-Fallback` (the model that answered, when it was a fallback).

Optional request headers: `X-SiliconRouter-Priority: high|normal|low` (when `queue.priority_header` is
on) and `Cache-Control: no-cache` (skip the response cache).

Key metrics: `siliconrouter_prefix_cache_routing_total{result="hit|rebalanced|miss"}`,
`siliconrouter_queue_depth`, `siliconrouter_queue_wait_seconds`, `siliconrouter_backend_inflight`,
`siliconrouter_backend_healthy`, `siliconrouter_retries_total`, `siliconrouter_request_duration_seconds`,
and, with chipagent, `siliconrouter_backend_accelerators{vendor}`,
`siliconrouter_backend_accelerator_memory_bytes` and `siliconrouter_backend_accelerator_utilization_percent`.

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
   `light → standard → heavy → reserve`. Within a tier, the least utilized backend wins; on a tie,
   the node whose accelerators chipagent reports as less busy wins.
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
agent/              chipagent (C++17): per-node CPU, GPU and accelerator telemetry, any vendor
scripts/            cross-architecture test runner for chipagent (QEMU)
deploy/             Helm chart, systemd units, Grafana dashboard, Docker Compose demo
internal/hardware   chipagent client, telemetry schema, automatic tiering
internal/server     HTTP proxy, retries, streaming, auth, metrics endpoints
internal/pool       backends, scheduler/queue, health checking
internal/affinity   prefix → backend LRU table
internal/cache      response cache (LRU, byte budget, TTL, request coalescing, persistence)
internal/cluster    multi-router sync of prefix affinity, usage and in-flight counts
internal/discovery  backends from DNS A/AAAA and SRV records
internal/tracing    W3C trace context and OTLP/HTTP span export
internal/limits     per-client limits and usage counters
internal/tlsconf    HTTPS with certificate hot reload
internal/prompt     single-pass request scanner, prefix hashing, size estimates, policy claims
internal/config     YAML config, defaults, validation
```

`make test` runs the Go suite with the race detector, and `make agent-test` runs the C++ suite.
`make cross` builds the router for 25 OS/CPU targets, and `make agent-cross` runs chipagent's tests
on 7 more CPU architectures under QEMU. The
router depends only on the Go standard library and `gopkg.in/yaml.v3`. chipagent depends only on
the C++ standard library and POSIX; it needs no CUDA, ROCm or vendor SDK to build.

## Status and roadmap

See [docs/ROADMAP.md](docs/ROADMAP.md) for what's built, what's next, and how the open-source and
commercial editions split.

## License

Apache 2.0. See [LICENSE](LICENSE).
