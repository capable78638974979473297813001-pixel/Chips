# Roadmap

## Built (v0.1)

- OpenAI-compatible reverse proxy covering chat, completions and embeddings, with streaming
- Per-backend concurrency limits and one fair FIFO queue, with 429 backpressure when the queue is full
- Prefix-cache-aware routing (conversation stickiness), with a guard against load imbalance
- Hardware tiers (heavy / standard / light / reserve) and context-size filtering
- Model discovery and model-based routing across mixed engines (vLLM, Ollama, llama.cpp, ...)
- Active and passive health checks, plus automatic retry on a different backend
- Prometheus metrics, an admin status endpoint, API-key auth, and graceful shutdown
- `mockllm` simulator and `srbench` load generator for demos without GPUs
- `chipagent` (C++): one telemetry schema across NVIDIA (NVML), AMD and Intel (sysfs) and Apple
  silicon, with automatic tiering (`tier: auto`) and a busy-GPU tie-break in the router
- Any HTTP AI API is forwarded (OpenAI, Anthropic-style, Ollama, TGI, Triton/KServe, Gemini-style,
  multipart, binary), with the model read from the body, URL or form
- Speed: first-token failover for stalled backends, latency-aware routing, response cache
  (embeddings and temperature 0), and a single-pass request scanner (2.2x lower router overhead)
- Customization: model aliases, named API keys with priority classes, cache controls
- Operations: built-in HTTPS (hot-swapped certificates, optional mTLS), admin keys, per-key
  concurrency/rate/daily-token limits, per-client usage for chargeback, live config reload
- chipagent 0.2: host CPU on any architecture, every PCI GPU/accelerator by vendor and kind,
  Windows (DXGI), Intel Macs (IOKit), Jetson. Verified on 9 CPU architectures and 3 OSes

## Next

| Feature | Why it matters |
|---|---|
| Real tokenizers (optional, per model) | Exact token counts for context limits and billing |
| Durable usage storage and quota state across restarts | Quotas survive a router restart; long-term chargeback without Prometheus |
| Queue-depth and KV-usage signals scraped from vLLM/SGLang `/metrics` | Route on real KV-cache pressure. Raw GPU memory from chipagent can't show it, because engines reserve memory up front |
| chipagent: live telemetry for Intel, Gaudi (hl-smi), Neuron, TPU; Apple/Windows GPU utilization; real-hardware validation | Fill the remaining gaps in the vendor matrix |
| gRPC and WebSocket proxying | Triton/KServe gRPC ports, realtime voice APIs |
| Shared routing state across router replicas (gossip or Redis) | Run the router itself highly available without losing cache affinity |
| OpenTelemetry tracing | Per-request timing: queue, TTFT, generation |
| Grafana dashboard | Shows the ROI story at a glance |

## Research track (hard, and not promised)

These came up in the original concept. They are real techniques, but they live **inside the
inference engine** or need deep integration with it. A proxy alone cannot do them:

- **Moving KV cache between servers.** Possible between engines of the same kind (e.g. vLLM with
  LMCache, or disaggregated prefill/decode in vLLM and SGLang). Moving KV cache between different
  vendors' hardware (NVIDIA → AMD → Apple) isn't practical, because cache layouts differ by engine,
  model and precision. Plan: integrate with LMCache/NIXL-based setups instead of reinventing them.
- **Prefill/decode split** (prefill on one node, decode on another). Same answer: orchestrate the
  engines that support it, and route the two phases to the right pools.
- **Speculative decoding.** It needs the draft and target models in the same engine, sharing
  tokenizers and running tight verification loops. vLLM and SGLang already support it. The router's
  role is to send traffic to backends configured for it, not to run a draft model itself.

Keeping this list honest matters when selling to infrastructure engineers. They will test every
claim.

## Open core: what's free vs. paid

**Free (Apache 2.0): everything a single team needs to run in production.** Everything in "Built"
above stays free. This is what gets developers to adopt it.

**SiliconRouter Enterprise (commercial license):**

- Multi-replica HA router with shared affinity state
- SSO/OIDC, per-team keys, quotas, chargeback reports and audit logs
- Policy controls: data residency rules, model allow-lists, PII redaction hooks
- KV-aware routing using live engine telemetry, plus LMCache/disaggregated-serving orchestration
- Management UI and fleet dashboards
- Support SLA and long-term support releases

**Services:** cluster design and tuning engagements, and benchmark-backed ROI reports on the
customer's own hardware.

**Go-to-market sequence:**

1. Publish benchmarks from real hardware: `srbench` on vLLM with an open model, router on vs. off.
2. Write integration guides for vLLM, Ollama and llama.cpp, plus a Helm chart.
3. Sell Enterprise to the teams that are already running the free version in production.
