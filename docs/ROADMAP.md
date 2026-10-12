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
- Cluster mode (shared prefix affinity and quotas across routers), state persistence, request
  coalescing, exact token counts near context limits, gRPC and WebSocket passthrough, OTLP
  tracing, log sampling, Grafana dashboard, Helm chart, systemd units, release builds, chipagent
  plugins
- Fleet operations: drain/undrain backends for maintenance, priority load shedding, model
  fallbacks, per-key model allow-lists and token caps, DNS/SRV backend discovery, and cluster-wide
  concurrency and rate limits
- Engine-aware routing from vLLM, SGLang, TGI, llama.cpp and Triton metrics (queue depth and KV
  usage), and demand-weighted cluster rate limits
- Reliability: stall detection from engine token counters, lost-GPU detection, retry budget,
  panic containment, graceful draining, a chaos test with leak checks, and `make soak`
- chipagent plugins for Gaudi, Inferentia/Trainium, Intel data center GPUs and Google TPUs, merged
  with chipagent's own PCI listing
- chipagent 0.2: host CPU on any architecture, every PCI GPU/accelerator by vendor and kind,
  Windows (DXGI), Intel Macs (IOKit), Jetson. Verified on 9 CPU architectures and 3 OSes
- Gateway API Inference Extension endpoint picker (ext_proc), tested with Envoy 1.35, and an
  InferencePool in the Helm chart
- LoRA-aware routing with on-demand adapter loading
- Scale-to-zero (requests held while backends boot, scale-up webhook) and per-model demand signals
  for KEDA and HPAs
- Single sign-on (OIDC/JWT) with teams and per-user quotas, and a hash-chained audit log
- Cost accounting in dollars per team and hardware
- Release hygiene: changelog, security policy, signed releases with SBOMs and provenance, Grafana
  dashboard for every metric, Prometheus alert rules with tests, and a quickstart with real vLLM

## Next

| Feature | Why it matters |
|---|---|
| Real-hardware validation beyond NVIDIA and AMD: Intel GPUs, Gaudi, Inferentia/Trainium, TPU, multi-GPU servers | The RTX 4090 and MI300X runs ([VALIDATION.md](VALIDATION.md)) found three bugs the fakes missed |

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
