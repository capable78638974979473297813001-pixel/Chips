# Real-hardware validation

What has run on real accelerators, what it showed, and what it changed. The checks are scripted
in [`scripts/hw-validate.sh`](../scripts/hw-validate.sh), so anyone can repeat them on a rented
GPU server with a vLLM image.

## October 2026, on RunPod

| Machine | GPU given to the container | Image |
|---|---|---|
| NVIDIA | GeForce RTX 4090 24 GB (driver 580, CUDA 13.0), Threadripper 7960X host | `vllm/vllm-openai` (vLLM 0.31) |
| AMD | Instinct MI300X 192 GB, one SR-IOV function of an eight-GPU host, EPYC 9474F | `rocm/vllm` ROCm 7.14.1, vLLM 0.23 |

Model: Qwen2.5-1.5B-Instruct. The whole test run cost about $4.

### chipagent against the vendor tools

After the fixes below, chipagent's readings match `nvidia-smi` and `rocm-smi`:

| Reading | RTX 4090, chipagent | `nvidia-smi` | MI300X, chipagent | `rocm-smi` |
|---|---|---|---|---|
| Devices | 1 | 1 | 1 | 1 |
| Memory total | 24,564 MiB | 24,564 MiB | 206,141,652,992 B | same |
| Memory used, idle | 0.94 MiB | 1 MiB | 297,771,008 B | same |
| Memory used, loaded | 17,418 MiB | 17,419 MiB | 146,726,395,904 B | same |
| Utilization, loaded | 100% | 100% | 5% | 5% |
| Temperature, loaded | 46 °C | 46 °C | 47 °C (junction) | 47 °C |
| Power, loaded | 269.06 W | 269.06 W | 224 W | 220 W (read a moment apart) |

chipagent's own test suite also passed on both machines.

**Bugs this found and fixed:**
1. **Every GPU in the host counted, not the container's.** On the MI300X machine chipagent
   reported all eight MI300X (1.5 TB) where the container had one: sysfs lists the whole host's
   devices. Devices with DRM or accel nodes now count only when their node is present in
   `/dev/dri` or `/dev/accel`. This affects any containerized deployment, Kubernetes included.
2. **No MI300X temperature.** The MI300X has no `temp1` sensor, only junction and memory. Sensors
   are now chosen by label: edge, then junction.
3. **NVIDIA memory used about 484 MiB too high.** NVML's v1 memory call counts memory the driver
   reserves for itself. chipagent now uses `nvmlDeviceGetMemoryInfo_v2` when the driver has it.

### Engine-aware routing

Two vLLM engines on the GPU, both behind the router with `metrics_path: /metrics`. 48 long
requests were sent straight to engine A, bypassing the router; A was capped at 16 sequences.

| | RTX 4090 | MI300X |
|---|---|---|
| Engine A reports | 16 running, 32 waiting | 16 running, 32 waiting |
| Router marks A | saturated | saturated |
| 20 requests through the router went to | B: 20 of 20 | B: 20 of 20 |

### Router overhead

The same engine, 384 or 288 multi-turn chat requests, directly and through the router:

| | Direct | Through the router |
|---|---|---|
| RTX 4090, long shared prompts | 34.7 req/s, p50 424 ms | 36.3 req/s, p50 385 ms |
| MI300X, long shared prompts | 43.2 req/s, p50 325 ms | 45.4 req/s, p50 324 ms |
| RTX 4090, long unique histories | 23.7 req/s, p50 430 ms | 23.5 req/s, p50 489 ms |

The differences are within run-to-run noise.

### Prefix-cache routing

Two engines sharing one RTX 4090. 48 conversations of 8 turns, each with a long unique history,
12 at a time. Each engine's KV cache held 38,544 tokens, so neither could keep every conversation
cached.

| | Prompt tokens served from cache | Tokens recomputed | Throughput | p50 | p95 | p99 |
|---|---|---|---|---|---|---|
| Prefix-cache routing on | **80.1%** | 213,787 | 16.9 req/s | 658 ms | 1.02 s | 1.11 s |
| Off (load only) | 61.6% | 412,372 | 16.6 req/s | 692 ms | 1.22 s | 1.38 s |

Routing each conversation back to the engine holding its history cut prompt recomputation by
48%, and tail latency by 16–20%. With a 1.5B model, prefill is cheap and both engines share one
GPU, so the latency gain is small. Larger models and separate GPUs would gain more.

**When there is no cache pressure, it doesn't help.** With a large shared system prompt and
plenty of KV cache (each engine could hold everything), the hit rate was the same either way
(96.3% against 95.9%), and throughput 8% lower with affinity, likely within noise. Prefix-cache
routing pays off when the KV cache can't hold every conversation on every server, which is the
usual case with large models under load.

### Mixed vendors

One router in front of the RTX 4090 and the MI300X, reached through RunPod's HTTPS proxy:
- Both backends were healthy, with chipagent telemetry and vLLM metrics from each.
- They tiered automatically from the hardware: RTX 4090 → standard, MI300X → heavy.
- Short prompts went to the RTX 4090; long prompts (over `large_request_tokens`) went to the
  MI300X.

### What went wrong in the testing, and the lessons

- **Two vLLM processes on one MI300X stalled** when both ran at once: the engines reported
  requests running but generated almost no tokens. The router held the requests until the
  client's 5-minute timeout. This came from the test setup (two engines sharing one SR-IOV GPU),
  not the router. But it shows that a hung engine holds non-streaming requests indefinitely
  unless `upstream.response_header_timeout` is set; see [TUNING.md](TUNING.md).
- **vLLM images must match the host driver.** `vllm/vllm-openai:latest` needs a CUDA 13 driver,
  and on a driver 570 host it failed with CUDA error 804. The `v0.31.0-cu129` image failed for a
  missing CUDA 13 library. Pin the image to the hosts' driver.

### Not covered yet

- Intel GPUs, Gaudi, Inferentia/Trainium and TPUs, which RunPod doesn't offer.
- Multi-GPU servers, and NVLink/xGMI topologies.
- Long runs.
- Larger models.
- chipagent on Windows and macOS with discrete GPUs.
