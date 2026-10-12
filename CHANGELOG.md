# Changelog

All notable changes to SiliconRouter (the router, `chipagent`, the Helm chart and the deploy
files). The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/). Before 1.0, minor versions may change config or
API behavior, and this file says how when they do.

The release workflow publishes the section for a tag as that release's notes.

## [Unreleased]

Found by the second round of real-hardware tests (2× RTX 4090, 2× A100; see docs/VALIDATION.md).

### Fixed
- Prefix-cache routing no longer sends every conversation that shares a system prompt to one
  backend. A prefix remembers up to four backends holding it, and a request goes to the least busy.
  On two GPUs this moved a soak from a 10,560 / 544 split to 2,964 / 2,988.
- A burst of first requests for a LoRA adapter loads it on one backend, not on every backend.
- Cost reports no longer undercount: requests that had finished since the last engine scrape
  kept a share of the backend's cost that was charged to no one.

### Added
- `agent_devices` backend setting: on a machine running one engine per GPU, each backend counts
  only its own GPU for tiering and faults.
- `srbench -stream` (reports time to first token), `-models` (one model or adapter per
  conversation) and `-timeout`.
- `scripts/hw-validate.sh` suites: repeated runs, a killed engine, a hung engine, a soak with a
  cost check, and LoRA adapters. mockllm answers `/health`.

## [0.4.0] - first tagged release

The first release with binaries, signed container images and a Helm chart. Everything below is
new, because no earlier version was published.

### Routing
- OpenAI-compatible reverse proxy that forwards any HTTP inference API: OpenAI, Anthropic-style,
  Ollama, TGI, Triton/KServe, Gemini-style, multipart and binary bodies, WebSocket and gRPC.
- One fair queue with per-backend concurrency limits and `429 Retry-After` backpressure, plus
  priority classes and priority load shedding.
- Prefix-cache-aware routing (conversation stickiness), with a guard against load imbalance.
- Hardware tiers (`heavy`, `standard`, `light`, `reserve`, or `auto` from chipagent) and
  context-size filtering, with exact token counts near context limits.
- Engine-aware routing from vLLM, SGLang, TGI, llama.cpp and Triton metrics: queue depth and KV
  usage.
- **LoRA-aware routing**: requests for an adapter go to servers that already have it loaded, then
  to servers with a free adapter slot. With `dynamic_lora`, the router loads adapters on demand
  through vLLM's runtime LoRA API.
- Latency-aware tie-breaks, first-token failover for stalled streams, and a response cache for
  embeddings and temperature-0 requests, with request coalescing.
- Model aliases and model fallbacks.

### Kubernetes and autoscaling
- **Gateway API Inference Extension endpoint picker**: `gateway.listen` serves Envoy's ext_proc
  gRPC protocol, so an InferencePool can use the router's routing decisions. It supports
  `FULL_DUPLEX_STREAMED` (plus `BUFFERED` and `NONE`), subset hints, immediate responses and
  gRPC health checking. It was tested with Envoy 1.35. The Helm chart can create the
  InferencePool.
- **Scale-to-zero**: `autoscaling.cold_start_wait` holds requests while a backend boots, and
  `scale_to_zero_models` may have no backends at all. A scale-up webhook asks for one.
- Per-model demand signals (in flight, queued, held, ready and desired backends) as metrics and as
  JSON at `/autoscale` for KEDA and HPAs, with a KEDA `ScaledObject` example.
- DNS and SRV backend discovery (headless Services, Consul), and cluster mode, which shares prefix
  placement, quotas and rate limits across routers.
- Helm chart with a StatefulSet, PodDisruptionBudget, ServiceMonitor, optional PrometheusRule,
  optional chipagent DaemonSet, and optional InferencePool.

### Enterprise
- **Single sign-on**: JWT access tokens from any OpenID Connect provider (JWKS discovery, key
  rotation, RS/PS/ES/EdDSA only). Groups map to teams with their own priority, limits and model
  policy. Admin groups are supported.
- **Per-user quotas** (`auth.oidc.user_limits`) on top of per-team and per-key limits: concurrency,
  requests per minute, tokens per day, allowed models and max tokens per request.
- **Audit log**: hash-chained JSON lines covering every request (no content), failed login, admin
  call and config change, with rotation. `siliconrouter -verify-audit` checks the chain.
- **Cost accounting in dollars**: `cost_per_hour` and token prices per backend. Each backend's cost
  is split between the requests running on it at each moment. `/admin/costs` reports spend per
  team, hardware and day, and idle cost is reported separately.
- HTTPS with certificate hot reload, optional mutual TLS, admin keys, live config reload
  (`SIGHUP`, `POST /admin/reload`), and drain/undrain for maintenance.

### Reliability and operations
- Active and passive health checks, retries on another backend, and a retry budget against retry
  storms.
- Stall detection from engine token counters, and lost-GPU detection from chipagent.
- Panic containment, graceful draining on shutdown, a chaos test with leak checks, and `make soak`.
- Prometheus metrics, OpenTelemetry tracing (OTLP/HTTP), sampled request logs, and state
  persistence (response cache, usage, costs) across restarts.
- Grafana dashboard covering traffic, reliability (stalls, GPU faults, retry budget), engine load,
  speed, autoscaling, LoRA, hardware and cost. It is generated by `make dashboard`.
- Prometheus alert rules for failed GPUs, stalled engines, backends down, error rate, retry budget,
  shedding, queue wait, stuck cold starts, panics, LoRA load failures, auth failures and idle spend,
  with `promtool` unit tests (`make alerts-test`).
- Quickstart with real engines: Docker Compose with two vLLM servers on a GPU, or two llama.cpp
  servers on the CPU, plus Prometheus with the alerts and Grafana with the dashboard.

### chipagent
- C++17 hardware agent with one telemetry schema across NVIDIA (NVML), AMD and Intel (sysfs),
  Apple silicon, Windows (DXGI), Intel Macs (IOKit) and Jetson. It lists every PCI GPU or
  accelerator by vendor and kind, has plugins for Gaudi, Inferentia/Trainium, Intel data center
  GPUs and TPUs, and works on 9 CPU architectures.

### Releases
- Binaries for Linux, macOS and Windows (amd64 and arm64), with `SHA256SUMS` signed by Sigstore
  cosign (keyless) and GitHub build provenance attestations.
- Multi-arch images on GHCR, signed with cosign, with SBOM and SLSA provenance attestations.
- An SPDX SBOM of the source and its dependencies. See [SECURITY.md](SECURITY.md) to verify any of
  these.

### Tested on
- An NVIDIA RTX 4090 and an AMD MI300X behind one router ([docs/VALIDATION.md](docs/VALIDATION.md)).
  Other accelerators are covered by simulators and fakes so far.

[Unreleased]: https://github.com/capable78638974979473297813001-pixel/Chips/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/capable78638974979473297813001-pixel/Chips/releases/tag/v0.4.0
