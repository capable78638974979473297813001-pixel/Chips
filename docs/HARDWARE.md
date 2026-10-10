# Mixing NVIDIA, AMD, Intel and Apple hardware

Every vendor reports its hardware differently. NVIDIA uses the NVML library, AMD's `amdgpu` driver
and Intel's drivers publish files under Linux sysfs, and Apple exposes unified memory through
`sysctl`. A team running all of them would normally write and maintain a separate inventory script
for each vendor, then hand-tag every server in the router config.

**chipagent** replaces that. It is a small C++17 program that runs on each GPU server, reads
whichever vendor interfaces are present, and serves one JSON schema. SiliconRouter polls it and:

- **Assigns tiers automatically.** A backend with `tier: auto` is classified from the hardware it
  reports, so adding a new node means adding its two URLs, not working out where it fits.
- **Sees load the router can't.** If a training job or another service is using a node's GPUs,
  the router can't see that load from its own request counts. When two nodes in a tier have the
  same load in the router, the one whose accelerators are at least 10 points less busy wins.
- **Shows one inventory.** `/admin/backends` and `/metrics` list every node's vendor, devices,
  memory and utilization in the same format, whatever the vendor.

## What chipagent reads

| Vendor | Interface | Reports |
|---|---|---|
| NVIDIA | NVML (`libnvidia-ml.so.1`, ships with the driver), loaded at runtime | name, UUID, memory total/used, GPU utilization, temperature, power |
| AMD | `amdgpu` sysfs: `/sys/class/drm/cardN/device` | name (from `product_name` or a built-in ID table), PCI address, VRAM total/used, `gpu_busy_percent`, hwmon temperature and power |
| Intel | DRM sysfs | detected with PCI ID; memory and utilization are not read yet |
| Apple silicon | `sysctl` + Mach VM statistics | chip name, unified memory total/used |
| Any NVIDIA card when NVML is missing | DRM sysfs | detected with PCI ID only |

The build needs no CUDA toolkit, ROCm or vendor SDK. NVML is loaded with `dlopen`, so the same
binary runs on NVIDIA, AMD, Intel and CPU-only Linux machines.

Status: the NVML path is tested against a stand-in library that exports the real NVML C functions,
and the sysfs path against a fake `/sys` tree. CI builds and tests on Linux and macOS (Apple
silicon). Real-hardware testing on each vendor is still to do; please report anything that reads
wrong on your machines.

## Run it

```bash
make agent                                   # cmake build into agent/build
agent/build/chipagent --once                 # print what this machine has, then exit
agent/build/chipagent --listen :9400         # serve GET /v1/telemetry and /healthz
```

Or in a container (`docker build -t chipagent agent/`): add `--gpus all` on NVIDIA hosts so the
NVIDIA runtime mounts NVML into the container. On AMD and Intel hosts no flags are needed because
`/sys` is visible to containers by default.

The agent serves read-only hardware telemetry with no authentication. Bind it to the cluster network,
for example `--listen 10.0.0.11:9400`, and don't expose it publicly.

`--simulate vendor:name:memory_gib[:utilization_percent]` (repeatable) reports fake devices, which is
what `make demo-mixed` uses to show an NVIDIA + AMD + Apple fleet on a laptop.

## Point the router at it

```yaml
backends:
  - name: gpu-node-4
    url: http://10.0.0.14:8000        # the inference server (vLLM, Ollama, ...)
    agent_url: http://10.0.0.14:9400  # chipagent on the same machine
    tier: auto
```

`agent_url` also works with a fixed tier: you get the inventory, metrics and the busy-GPU
tie-break, and your tier is kept.

### How `tier: auto` decides

From the total accelerator memory on the node (summed across GPUs):

| Hardware | Tier |
|---|---|
| ≥ `heavy_min_memory_gib` (default 64): H100/H200, MI300X, 4× L4 | heavy |
| ≥ `standard_min_memory_gib` (default 20): L4, A10, RTX 4090, Radeon 7900 | standard |
| less than that: T4, small cards | light |
| accelerators found but the driver doesn't report memory | standard |
| no accelerators (CPU inference) | light |
| unified memory (Apple silicon) | at most standard |

Apple silicon is capped because a Mac Studio can hold a large model but has a fraction of HBM's
memory bandwidth, so it generates tokens more slowly than a data-center GPU. Set an explicit
tier on a backend to override any of this.

Until a node's agent first answers, an auto backend is treated as `standard`. If an agent stops
answering later, the router keeps that node's last tier and inventory, stops using its stale
utilization, and shows the error under `hardware.agent_error`. Agent problems never take a backend
out of rotation; only the inference health check does that.

## Telemetry schema (v1)

`GET /v1/telemetry`:

```json
{"schema":1,"agent":"chipagent","version":"0.1.0","hostname":"gpu-node-1","devices":[
  {"index":0,"vendor":"amd","name":"AMD Instinct MI300X","id":"0000:03:00.0","source":"sysfs",
   "memory_total_bytes":206158430208,"memory_used_bytes":1073741824,"unified_memory":false,
   "utilization_percent":42,"temperature_c":51,"power_watts":350}]}
```

Readings a driver doesn't provide are `null`. `agent/tests/testdata/telemetry.json` is checked
by both the C++ tests (which must produce it byte for byte) and the Go tests (which must decode
it). If a change breaks either side, CI fails.

## Router metrics

- `siliconrouter_backend_accelerators{backend,vendor}`
- `siliconrouter_backend_accelerator_memory_bytes{backend,state="total|used"}`
- `siliconrouter_backend_accelerator_utilization_percent{backend}`

One caution about memory used: vLLM and SGLang reserve most of GPU memory for KV cache when they
start, so `used` stays near 90% on a busy or idle node alike. That's why the router uses it for
inventory only and doesn't route on it. Utilization is the routing signal.
