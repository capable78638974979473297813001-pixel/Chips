# Compatibility: which chips, which AI

SiliconRouter never runs a model itself. It forwards HTTP requests to inference servers and picks
which one handles each request. Any machine that can run an inference server can therefore be a
backend, whatever chip it uses. This page lists what that covers in practice and where the limits are.

## AI software (backends)

The router forwards **every** request path except its own (`/healthz`, `/readyz`, `/metrics`,
`/admin/*`, and `GET /v1/models`), so any AI service that speaks HTTP works. For the APIs below, it
also reads the model name and prompt. That lets it send a request to a backend serving that model,
and keep a conversation on the server that already has it cached.

| API style | Used by | Model taken from | Prompt-cache routing |
|---|---|---|---|
| OpenAI (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, ...) | vLLM, SGLang, llama.cpp, Ollama, LM Studio, TGI, LocalAI, TensorRT-LLM, OpenVINO Model Server, hosted OpenAI-compatible APIs | body `model` | ✓ |
| Anthropic Messages (`/v1/messages`) | engines that offer an Anthropic-compatible endpoint | body `model` | ✓ (`system` included) |
| Ollama native (`/api/chat`, `/api/generate`, `/api/embed`) | Ollama | body `model` | ✓ |
| Hugging Face TGI native (`/generate`, `/generate_stream`) | TGI | – (one model per server) | ✓ |
| Gemini / Vertex style (`/v1beta/models/{m}:generateContent`) | Gemini-compatible servers | URL | ✓ |
| Triton / KServe v2 (`/v2/models/{m}/infer`) | NVIDIA Triton, KServe, Seldon MLServer, OpenVINO | URL | – (tensor inputs) |
| KServe v1 / TF Serving (`/v1/models/{m}:predict`) | TensorFlow Serving, KServe | URL | – |
| Azure OpenAI (`/openai/deployments/{m}/...`) | Azure-compatible gateways | URL | ✓ |
| Multipart uploads (speech-to-text, image edits) | Whisper servers, image models | form field `model` | – |
| gRPC (`application/grpc`) | Triton and KServe gRPC ports, other gRPC model servers | – (routed by load to backends with `grpc_url`) | – |
| WebSocket (realtime APIs) | OpenAI-style realtime voice/text sessions | `?model=` or URL | – |
| Anything else (custom JSON, protobuf, binary tensors) | TorchServe, BentoML, Ray Serve, in-house services | – | – (routed by load) |

Requests the router can't parse are never rejected; they are routed by load alone. Bodies are
forwarded byte for byte.

**Per-backend settings** for non-OpenAI servers:

```yaml
backends:
  - name: triton-1
    url: http://10.0.0.20:8000
    health_path: /v2/health/ready   # TGI: /health or /info · Ollama: /api/tags · Ray Serve: /-/healthz
  - name: hosted-api
    url: https://api.example.com
    api_key_env: HOSTED_API_KEY
    api_key_header: x-api-key       # default sends "Authorization: Bearer <key>"
```

Clients may authenticate to the router with `Authorization: Bearer`, `x-api-key` (Anthropic SDKs)
or `api-key` (Azure SDKs).

**gRPC.** Give each gRPC-capable backend its gRPC address (Triton and KServe use port 8001 next
to HTTP on 8000):

```yaml
backends:
  - {name: triton-1, url: http://10.0.0.20:8000, grpc_url: http://10.0.0.20:8001}
```

Calls are relayed over HTTP/2: cleartext h2c for `http://`, TLS for `https://`. Trailers such as
`grpc-status` pass through. Clients may connect to the router with TLS, or with cleartext HTTP/2.

**Engine load metrics.** With `metrics_path: /metrics`, the router also routes on what the engine
reports about its own queue and KV cache (see [TUNING.md](TUNING.md)). It understands vLLM,
SGLang, TGI, llama.cpp (started with `--metrics`) and Triton. Other servers are routed on the
router's own counts.

**WebSocket.** Upgrades are relayed to a backend chosen by model and load. The session occupies
one of the backend's `max_concurrency` slots until it closes.

Neither gRPC nor WebSocket can be retried on another backend once started, and their contents
aren't inspected, so prefix-cache routing and the response cache don't apply to them.

## Chips

### The router

Pure Go with no C dependencies. `make cross` builds it for 25 OS/CPU targets:

- **Linux:** x86 (32/64-bit), Arm (32/64-bit), POWER (ppc64, ppc64le), IBM Z (s390x), RISC-V, MIPS (all four variants), LoongArch
- **Windows:** x86, x64, Arm64
- **macOS:** Intel and Apple silicon
- **Other Unix:** FreeBSD, OpenBSD, NetBSD, illumos, AIX

The router's tests also pass when run under emulated Arm64 and big-endian IBM Z.

### Inference hardware

The router only sends HTTP, so the chips doing the inference can be anything your inference
software supports. For example:

- NVIDIA (CUDA)
- AMD (ROCm)
- Intel (oneAPI / OpenVINO / Gaudi)
- Apple (Metal)
- AWS Inferentia/Trainium (Neuron)
- Google TPU
- Huawei Ascend
- Qualcomm Cloud AI
- Tenstorrent
- plain CPUs on every architecture listed above

chipagent identifies most of these automatically; see [HARDWARE.md](HARDWARE.md) for what it reads
from each and which platforms are verified.

### Honest limits on "every chip from the last 30 years"

- **Graphics chips from before about 2007** (3dfx, S3, Riva 128, early Radeon/GeForce) can't run
  AI compute at all; CUDA arrived in 2007 and OpenCL in 2009. chipagent lists them as `display` so
  they appear in the inventory, and the router never counts them as capacity.
- **Old CPUs** can run small quantized models with llama.cpp, but slowly. A Pentium-era machine is
  still a valid backend; it just tiers as `light` and gets small requests.
- **chipagent needs** Linux (any architecture with a C++17 compiler), Windows 7 or later, or macOS.
  On other systems, set the backend's tier by hand; the router works without the agent.
- **Phones and laptop NPUs** (Snapdragon, Apple Neural Engine, Ryzen AI) are only usable through
  whatever HTTP server runs on them. chipagent doesn't detect NPUs built into a system-on-chip,
  apart from NVIDIA Jetson.
- **None of this has run on real accelerator hardware yet.** Coverage comes from fakes and
  emulation; see the verification table in [HARDWARE.md](HARDWARE.md).
