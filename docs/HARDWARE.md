# Mixing hardware from any vendor

Every vendor reports its hardware differently. NVIDIA uses NVML, AMD publishes files under Linux
sysfs, Windows exposes GPUs through DXGI, Apple uses `sysctl`, and most AI accelerators (Gaudi,
Inferentia, TPU, Ascend, ...) have no common monitoring interface at all. A team running several
of them would normally maintain a separate inventory script per vendor and hand-tag every server
in the router config.

**chipagent** replaces that. It is a small C++17 program that runs on each server, reads
whichever interfaces are present, and serves one JSON schema. SiliconRouter polls it and:

- **Assigns tiers automatically.** A backend with `tier: auto` is classified from the hardware it
  reports, so adding a new node means adding its two URLs, not working out where it fits.
- **Sees load the router can't.** If a training job or another service is using a node's GPUs,
  the router can't see that load from its own request counts. When two nodes in a tier have the
  same load in the router, the one whose accelerators are at least 10 points less busy wins.
- **Shows one inventory.** `/admin/backends` and `/metrics` list every node's CPU, devices,
  memory and utilization in the same format, whatever the vendor.

## What chipagent detects

**Every machine** reports its host: OS, CPU architecture, CPU model, core count, RAM, and the CPU
instruction-set extensions that matter for CPU inference (AVX2/AVX-512/AMX on x86, NEON/SVE on Arm,
AltiVec/VSX on POWER, vector facilities on IBM Z, RVV on RISC-V, LSX/LASX on LoongArch). A machine
with no accelerator is still capacity for CPU inference (llama.cpp and similar).

**Every PCI GPU and AI accelerator** is detected by its PCI vendor ID and class code, including
makers chipagent has never heard of. Each device gets a `kind`:

| Kind | Meaning | Examples |
|---|---|---|
| `gpu` | general-purpose GPU | NVIDIA, AMD, Intel Arc, Qualcomm, unknown vendors' GPUs |
| `npu` | neural processing unit | Intel NPU, Huawei Ascend |
| `accelerator` | dedicated AI or compute card | Intel Gaudi, AWS Inferentia/Trainium, Google TPU, Tenstorrent, FPGAs |
| `display` | display-only, **not** AI capacity | server BMC graphics (ASPEED, Matrox), VM adapters (QEMU, VMware, Hyper-V, VirtualBox, virtio), and 1990s–2000s chips from before GPU compute existed (3dfx, S3, Riva 128, ...) |

How much detail each vendor gets:

| Hardware | Read through | Identity | Memory | Utilization | Temp/power |
|---|---|---|---|---|---|
| NVIDIA (Linux, Windows) | NVML, loaded at runtime | ✓ | ✓ | ✓ | ✓ |
| AMD (Linux) | `amdgpu` sysfs | ✓ | ✓ | ✓ | ✓ |
| NVIDIA Jetson (Linux) | device tree | ✓ | ✓ (shared RAM) | – | – |
| Apple silicon (macOS) | `sysctl` + Mach | ✓ | ✓ (unified) | – | – |
| Any GPU on Windows 7+ | DXGI | ✓ | ✓ (dedicated VRAM) | – | – |
| Intel Macs' GPUs | IOKit | ✓ | where macOS reports it | – | – |
| Every other PCI device (Linux) | PCI sysfs | ✓ | – | – | – |

| Anything with a vendor tool | a plugin (see below) | ✓ | what the tool reports | ✓ | ✓ |

"–" means chipagent reports `null`, and the router treats the reading as unknown. A device whose
memory is unknown is still counted, and tiers as `standard`.

No vendor SDK is needed to build or run it. NVML is loaded at runtime when it is present, so one
binary covers machines with and without NVIDIA drivers.

### Where it is verified

| Platform | How it was tested |
|---|---|
| Linux x86_64 | Full test suite, with AddressSanitizer/UBSan |
| Linux aarch64, armv7, i686, ppc64le, s390x, riscv64, mips | Full test suite under QEMU emulation (`make agent-cross`, run in CI). s390x and MIPS are big-endian. |
| Windows x86_64 | Full test suite, both MinGW (under Wine) and MSVC (CI) |
| macOS, Apple silicon | Full test suite (CI) |
| macOS, Intel | Compiled; the IOKit GPU path has not run on an Intel Mac |
| **NVIDIA GeForce RTX 4090** (driver 580, container) | Real hardware: suite passes, readings match `nvidia-smi` idle and under load ([VALIDATION.md](VALIDATION.md)) |
| **AMD Instinct MI300X** (ROCm, SR-IOV container) | Real hardware: suite passes, readings match `rocm-smi` idle and under load |

The first real-hardware run found three bugs, all fixed, which the fakes had missed:
- in containers, every GPU in the host was counted;
- the MI300X temperature was missing;
- NVIDIA memory used was about 484 MiB too high.

Other vendor code paths are tested against fakes: a stand-in NVML library that exports the real
C functions, and fake `/sys` and `/proc` trees, including `/proc/cpuinfo` samples from 12 CPU
families. **Intel GPUs, Gaudi, Inferentia/Trainium and TPUs have not run on real hardware yet.**
Please report anything that reads wrong on your machines.

**In containers,** chipagent counts only the GPUs and accelerators whose `/dev/dri` or
`/dev/accel` nodes the container was given, so on a shared host it reports its own GPUs, not the
whole machine's. Run it with the host's `/dev` (or with no `/dev/dri` at all) to inventory the
whole machine.

### When hardware misbehaves

- **Hung reads.** A wedged GPU can make driver or sysfs reads block indefinitely. chipagent
  collects on a worker thread. If fresh readings don't arrive within `--collect-timeout` (default
  1500 ms, keep it below the router's `hardware.timeout`), it answers with its last good readings,
  marked `"stale": true` with a `collect_error`. It doesn't start more reads while one is stuck.
- **Lost GPUs.** An NVIDIA GPU that falls off the bus (NVML reports it lost, Xid 79) is listed
  with an `error` instead of vanishing. The router takes that machine out of rotation until a
  healthy report arrives, and lists the fault in `/admin/backends` and
  `siliconrouter_backend_hardware_faults`.

### Plugins: any other chip

For hardware without built-in readings, chipagent runs a plugin: any command that prints the
machine's devices as JSON in chipagent's schema.

```bash
chipagent --plugin /opt/chipagent/plugins/gaudi-hl-smi.sh --plugin "/opt/chipagent/plugins/aws-neuron.py"
```

Four plugins are included:
- Intel Gaudi, via `hl-smi`;
- AWS Inferentia and Trainium, via `neuron-ls`;
- Intel data center GPUs (Max, Flex), via `xpu-smi`;
- Google Cloud TPUs, from PCI plus `tpu-info` for HBM use and duty cycle.

A plugin's device and chipagent's own PCI listing of the same card are merged by PCI address, so
nothing is counted twice.

Writing one for another accelerator is a short script around its vendor tool. A plugin that hangs
is killed after `--plugin-timeout` (default 5 s), and bad output is logged and skipped. See
[`agent/plugins/README.md`](../agent/plugins/README.md). The included plugins have been tested
against sample output of the vendor tools, not yet on real hardware.

## Run it

```bash
make agent                                   # cmake build into agent/build
agent/build/chipagent --once                 # print what this machine has, then exit
agent/build/chipagent --listen :9400         # serve GET /v1/telemetry and /healthz
```

On Windows, build with Visual Studio or MinGW (`cmake -S agent -B build && cmake --build build`)
and run `chipagent.exe --listen :9400`.

Or in a container (`docker build -t chipagent agent/`): add `--gpus all` on NVIDIA hosts so the
NVIDIA runtime mounts NVML into the container. On other Linux hosts no flags are needed because
`/sys` is visible to containers by default.

The agent serves read-only hardware telemetry with no authentication. Bind it to the cluster network,
for example `--listen 10.0.0.11:9400`, and don't expose it publicly.

`--simulate vendor:name:memory_gib[:utilization_percent[:kind]]` (repeatable) reports fake devices,
which is what `make demo-mixed` uses to show an NVIDIA + AMD + Apple fleet on a laptop.

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
| accelerators found but memory unknown (Gaudi, TPU, Inferentia, ...) | standard |
| no accelerators, or display adapters only (CPU inference) | light |
| unified memory (Apple silicon, Jetson) | at most standard |

Unified-memory machines are capped because they can hold a large model but have a fraction of
HBM's memory bandwidth, so they generate tokens more slowly than a data-center GPU. Set an explicit
tier on a backend to override any of this.

Until a node's agent first answers, an auto backend is treated as `standard`. If an agent stops
answering later, the router keeps that node's last tier and inventory, stops using its stale
utilization, and shows the error under `hardware.agent_error`. Agent problems never take a backend
out of rotation; only the inference health check does that.

## Telemetry schema (v1)

`GET /v1/telemetry`:

```json
{"schema":1,"agent":"chipagent","version":"0.2.0","hostname":"gpu-node-1",
 "host":{"os":"linux","arch":"x86_64","cpu":"AMD EPYC 9654 96-Core Processor","logical_cpus":192,
         "memory_total_bytes":1649267441664,"memory_used_bytes":68719476736,
         "cpu_features":["avx2","avx512f","avx512_bf16"]},
 "devices":[
  {"index":0,"vendor":"amd","kind":"gpu","name":"AMD Instinct MI300X","id":"0000:03:00.0","source":"sysfs",
   "memory_total_bytes":206158430208,"memory_used_bytes":1073741824,"unified_memory":false,
   "utilization_percent":42,"temperature_c":51,"power_watts":350},
  {"index":1,"vendor":"aspeed","kind":"display","name":"ASPEED BMC graphics [1a03:2000]", ...}]}
```

Readings a driver doesn't provide are `null`. Version 0.2 added `host` and `kind`. The router still
accepts 0.1 reports and treats their devices as GPUs. `agent/tests/testdata/telemetry.json` is checked
by both the C++ tests (which must produce it byte for byte) and the Go tests (which must decode
it). If a change breaks either side, CI fails.

## Router metrics

- `siliconrouter_backend_accelerators{backend,vendor,kind}`
- `siliconrouter_backend_accelerator_memory_bytes{backend,state="total|used"}`
- `siliconrouter_backend_accelerator_utilization_percent{backend}`

One caution about memory used: vLLM and SGLang reserve most of GPU memory for KV cache when they
start, so `used` stays near 90% on a busy or idle node alike. That's why the router uses it for
inventory only and doesn't route on it. Utilization is the routing signal.
