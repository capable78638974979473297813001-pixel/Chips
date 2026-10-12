# Real-hardware validation

What has run on real accelerators, what it showed, and what it changed. The checks are scripted
in [`scripts/hw-validate.sh`](../scripts/hw-validate.sh), so anyone can repeat them on a rented
GPU server with a vLLM image.

## Round 2: separate GPUs, larger models, failures

| Machine | GPUs | Model |
|---|---|---|
| 2× RTX 4090 | two 24 GB GPUs on one PCIe switch, Ryzen 9 7950X host (EPYC 7282 for the re-run) | Qwen2.5-7B-Instruct |
| 2× A100-SXM4-80GB | NVLink (NV12), EPYC 7742 host | Qwen2.5-32B-Instruct |

One vLLM 0.31 engine per GPU (`CUDA_VISIBLE_DEVICES`), each capped at 16 sequences, both behind
the router. Each 4090 engine had a KV cache of 81,552 tokens, each A100 engine 41,392 to 45,904.
The run cost $3.61.

### Prefix-cache routing on separate GPUs

48 conversations of 8 turns, each with a long unique history (system prompt 2,000 characters,
user turns 3,000 on the 4090s and 2,000 on the A100s), 16 or 12 at a time. Neither engine could
hold every conversation. On the 4090s the router ran four times with prefix-cache routing and four
times without, alternating; on the A100s three times each.

| | Throughput | p50 | p95 | p99 | Prompt tokens from cache |
|---|---|---|---|---|---|
| **2× RTX 4090, 7B, on** (4 runs) | **12.7 req/s** (all four) | 1.25 s | 1.32–1.33 s | 1.34 s | 81.4% |
| 2× RTX 4090, 7B, off (4 runs) | 8.1–9.0 req/s | 1.39–1.87 s | 2.76–3.11 s | 3.44–4.24 s | 65.3–67.6% |
| One RTX 4090 alone | 9.2 req/s | 1.74 s | 1.89 s | 1.91 s | 81.4% |
| **2× A100, 32B, on** (3 runs) | **4.2 req/s** (all three) | 2.84–2.87 s | 2.97–2.99 s | 2.99–3.01 s | 82.7% |
| 2× A100, 32B, off (3 runs) | 2.7–2.9 req/s | 4.32–4.66 s | 5.44–5.83 s | 5.86–6.65 s | 66.0–66.7% |
| One A100 alone | 3.4 req/s | 3.62 s | 3.78 s | 3.81 s | 82.7% |

- **Routing on the cache added 41–57% throughput and cut p99 latency by half to two-thirds.** Runs
  with it on were nearly identical; runs without it varied.
- **Without it, two GPUs were no faster than one, and with the 32B model slower** (2.7–2.9 req/s
  against 3.4). Spreading turns of a conversation across engines recomputes its history each time
  it moves, which costs more than the second GPU adds. With it, each engine reused as much as a
  single engine does (81.4%, 82.7%).
- The gain grew with the model, as round 1 predicted: prefill is what a cache hit saves.

### A killed engine

Streaming load (64 conversations of 4 turns, 256 tokens a reply, 16 at a time) across both 4090
engines. 15 seconds in, engine B was killed with SIGKILL, then restarted.

- The router marked B unhealthy **within a second of the kill**, from the first refused connection.
- **8 requests failed: the streams B was generating when it died.** The router can't move a
  stream once tokens have reached the client. The other 228 succeeded, including 6 that reached B
  after it died and were retried on A.
- vLLM restarted in 30 s (weights cached). B was back in rotation as soon as its health check
  passed, and the next requests were split between both again.

### A hung engine

The same load; this time B's engine-core process was frozen with SIGSTOP for 45 seconds, then
resumed. B's API server kept running, so **`/health` kept answering 200** and `/metrics` kept
reporting 8 requests running, while its token counter stopped. Health checks cannot see this; only
stall detection can.

| | 4090s, `stall_after` 60 s (default) | A100s, `stall_after` 15 s |
|---|---|---|
| Stall detected | no (45 s is under the window) | **about 17 s after the freeze** |
| Back in rotation after resuming | n/a | within 2 s |
| Failed requests | 0 | 0 |
| Requests B was generating | finished late (p99 48 s) | finished late (p99 55 s) |

In both runs one new request reached B during the freeze; it got no first token within
`first_token_timeout` (10 s), and the router moved it to A. The default 60 s window is deliberately
long: a single very long prompt being prefilled also generates no tokens for a while. With short
prompts, a shorter window is safe.

### LoRA adapters

8 public LoRA adapters for Qwen2.5-7B (ranks 8–32), both engines with `--max-loras 4`, so all
eight fit only if each engine keeps its own four. 64 conversations of 4 turns, 24 at a time, each
conversation using one adapter. LoRA-aware routing with `dynamic_lora`, compared with every adapter
loaded on both engines (routing on load alone):

| | Adapter loads | Throughput | p50 | p95 | p99 |
|---|---|---|---|---|---|
| **LoRA-aware, loaded on demand** | **8, four per engine** | **14.4 req/s** | 1.53 s | 1.67 s | 1.71 s |
| Every adapter on both engines (2 runs) | 16 | 10.2–10.8 req/s | 1.56–1.61 s | 4.02–4.19 s | 4.77–5.49 s |

Routing each adapter to the engine holding it gave 33–41% more throughput and a third of the p99:
an engine juggling eight adapters in four slots makes requests wait for a slot.

### chipagent on more hardware

chipagent 0.3 matched `nvidia-smi` on both machines, per GPU, idle and loaded. Loaded, with GPU 0
flooded and GPU 1 idle:

| | GPU 0: chipagent | `nvidia-smi` | GPU 1: chipagent | `nvidia-smi` |
|---|---|---|---|---|
| RTX 4090 memory used | 20,175.1 MiB | 20,176 MiB | 20,971.1 MiB | 20,972 MiB |
| RTX 4090 utilization, temperature, power | 100%, 64 °C, 325.97 W | same | 0%, 45 °C, 59.95 W | same |
| A100-SXM4 memory used | 74,258.8 MiB | 74,259 MiB | 75,170.8 MiB | 75,171 MiB |
| A100-SXM4 utilization, temperature, power | 100%, 53 °C, 358.78 W | same | 0%, 55 °C, 99.51 W | same |

It listed the A100 machine's ASPEED BMC as a display adapter, not AI capacity. Its test suite
passed on both.

### Long run and cost accounting

A 20-minute soak on the 4090s: two teams (12 and 4 conversations at a time), 11,104 requests.
- 0 failed. Router goroutines stayed at 62–80 and its heap between 6 and 16 MiB, with no growth.
  The p50 latency of each benchmark round stayed between 1.63 and 1.84 s, with no drift.
- With `cost_per_hour` set to the pod's $1.78, the cost report came to $0.5991 for a run the clock
  put at $0.5971 plus the seconds before the soak began. Teams were charged in proportion to their
  traffic.

### Bugs this found and fixed

1. **Prefix-cache routing put 95% of the soak on one GPU** (10,560 requests against 544; 42% of the
   cost was idle). Each prefix remembered only the backend that served it last, and every
   conversation started with the same system prompt, so every new conversation went there until it
   was 50% busier than the other. A prefix now remembers up to four backends, and a request goes to
   the least busy of those holding its longest cached prefix. A 10-minute re-run: **2,964 against
   2,988 requests, 1.8% idle**, and p50 latency 1.60–1.69 s against 1.63–1.84 s, on a slower host.
   The cache-pressure benchmark routed the same as before (364 of 384 requests to their cache, 368
   before).
2. **A burst of first requests for an adapter loaded it on every engine.** The router noted an
   adapter only when its load finished, so concurrent first requests each found no engine with it.
   A load in progress now counts. Re-run: 8 loads for 8 adapters (the numbers above).
3. **Cost reports undercounted**, by 26% in a test with short requests. Requests sharing a backend
   were counted from the engine's last scrape, which still included requests that had finished
   since. Found while building the soak check, before the GPU run, which then reconciled.
4. **Every backend on a multi-GPU machine counted all its GPUs.** With one engine per GPU, a
   backend was tiered by the machine's total memory, and one failed GPU would have taken every
   engine on the machine out of rotation. The new `agent_devices` setting gives each backend its
   own GPU; on both machines each backend then reported exactly its GPU's readings.

The first round's checks are below.

## Round 1: October 2026, on RunPod

| Machine | GPU given to the container | Image |
|---|---|---|
| NVIDIA | GeForce RTX 4090 24 GB (driver 580, CUDA 13.0), Threadripper 7960X host | `vllm/vllm-openai` (vLLM 0.31) |
| AMD | Instinct MI300X 192 GB, one SR-IOV function of an eight-GPU host, EPYC 9474F | `rocm/vllm` ROCm 7.14.1, vLLM 0.23 |

Model: Qwen2.5-1.5B-Instruct. The run cost about $4.

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
- AMD with more than one GPU (no pair of MI300X was available for round 2), and xGMI.
- Tensor-parallel engines spanning several GPUs, and models over 32B.
- Runs longer than 20 minutes.
- chipagent on Windows and macOS with discrete GPUs.
