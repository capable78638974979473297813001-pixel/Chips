// chipagent: a vendor-neutral view of the compute hardware in one machine.
//
// Each vendor exposes its hardware differently: NVIDIA through the NVML
// library, AMD through Linux sysfs, Windows GPUs through DXGI, Apple through
// sysctl and Mach, and every other PCI accelerator (Gaudi, Inferentia, TPU,
// Ascend, ...) only through its PCI identity. chipagent reads all of them,
// plus the host CPU on any architecture, and reports one schema, so the
// router can treat an H100, an MI300X and an M2 Ultra as interchangeable
// capacity.
#pragma once

#include <atomic>
#include <cstdint>
#include <functional>
#include <memory>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

namespace chipagent {

// What a device can do for AI workloads.
//   gpu:         general-purpose GPU
//   npu:         neural processing unit built into a client chip
//   accelerator: dedicated AI/compute card (Gaudi, Inferentia, TPU, Ascend, FPGA, ...)
//   display:     display-only adapter (server BMC, virtual machine, pre-compute legacy
//                GPU); listed for inventory, never counted as AI capacity
struct Device {
  std::string vendor;  // "nvidia", "amd", "intel", "apple", ...
  std::string kind = "gpu";
  std::string name;    // marketing name when the driver reports one
  std::string id;      // stable identifier: NVML UUID or PCI bus address
  std::string source;  // where the data came from: nvml, sysfs, dxgi, iokit, sysctl, devicetree, simulated
  uint64_t memory_total_bytes = 0;  // 0 = unknown
  uint64_t memory_used_bytes = 0;
  // True when the accelerator shares system RAM (Apple silicon) instead of
  // having its own VRAM/HBM.
  bool unified_memory = false;
  std::optional<double> utilization_percent;
  std::optional<double> temperature_c;
  std::optional<double> power_watts;
};

// The machine itself. CPU inference (llama.cpp and friends) runs on any of
// these, so a box with no accelerator is still capacity.
struct Host {
  std::string hostname;
  std::string os;    // linux, darwin, windows, freebsd, ...
  std::string arch;  // x86_64, aarch64, armv7l, ppc64le, s390x, riscv64, ...
  std::string cpu;   // model name when the OS reports one
  unsigned logical_cpus = 0;
  uint64_t memory_total_bytes = 0;
  uint64_t memory_used_bytes = 0;
  // Instruction-set extensions that matter for CPU inference speed
  // (avx2, avx512f, amx_tile, asimd, sve, altivec, rvv, ...).
  std::vector<std::string> cpu_features;
};

// A Provider reads one vendor's devices. Devices() re-reads live values on
// every call.
class Provider {
 public:
  virtual ~Provider() = default;
  virtual std::string Name() const = 0;
  virtual std::vector<Device> Devices() = 0;
};

// NVIDIA GPUs through NVML. The library is loaded at runtime, so the agent
// builds without the CUDA toolkit and still runs on machines without NVIDIA
// drivers. Returns nullptr when NVML is missing or fails to initialize.
// An empty library means the driver's default libnvidia-ml.so.1.
std::unique_ptr<Provider> OpenNvml(const std::string& library);

// Every GPU and accelerator on the Linux PCI bus under root (normally "/"),
// with live memory, load and sensor readings where the driver publishes them
// in sysfs (amdgpu does). NVIDIA cards are skipped when skip_nvidia is set
// because NVML already covers them in more detail.
std::unique_ptr<Provider> OpenPci(const std::string& root, bool skip_nvidia);

// SoC GPUs that are not on PCI, found through the Linux device tree under
// root: NVIDIA Jetson (Tegra) modules.
std::unique_ptr<Provider> OpenDeviceTree(const std::string& root);

// Windows: every GPU through DXGI (Windows 7 and later). nullptr elsewhere.
std::unique_ptr<Provider> OpenDxgi(bool skip_nvidia);

// macOS: the Apple silicon GPU and its unified memory, or on Intel Macs the
// PCI GPUs through IOKit. nullptr on other platforms.
std::unique_ptr<Provider> OpenApple();

// What a PCI vendor/device/class triple is, for the PCI and DXGI providers.
struct PciIdentity {
  std::string vendor;  // empty: not an AI-relevant device, skip it
  std::string kind;
  std::string name;
};
PciIdentity ClassifyPci(unsigned vendor, unsigned device, unsigned class_code);

// The host CPU, memory and OS. root is "/" except in tests (Linux reads
// root/proc/cpuinfo and root/proc/meminfo).
Host ReadHost(const std::string& root);

// Pulls a CPU name and AI-relevant features out of Linux /proc/cpuinfo text,
// whose format differs on every architecture.
void ParseCpuinfo(const std::string& text, std::string* name, std::vector<std::string>* features);

// Devices reported by an external command that prints JSON (see plugin.cpp
// for the format). The command runs on each collection, with a timeout.
std::unique_ptr<Provider> OpenPlugin(const std::string& command, int timeout_ms);

// Runs cmd through the shell, capturing standard output. false (and err)
// on a non-zero exit or after timeout_ms (the command is then killed).
bool RunCommand(const std::string& cmd, int timeout_ms, std::string* out, std::string* err);

// Fixed devices for demos and tests. Each spec is
// "vendor:name:memory_gib[:utilization_percent]".
std::unique_ptr<Provider> OpenSimulated(const std::vector<std::string>& specs, std::string* err);

// Devices from every provider, in provider order.
std::vector<Device> Collect(const std::vector<std::unique_ptr<Provider>>& providers);

// The JSON document served at /v1/telemetry.
std::string RenderTelemetry(const std::vector<Device>& devices, const Host& host,
                            const std::string& version);

std::string JsonEscape(std::string_view s);

// A minimal HTTP/1.1 server: one request per connection, handled serially.
// It only serves small telemetry documents to the router, so it favors
// having no dependencies over throughput.
class HttpServer {
 public:
  struct Response {
    int status = 200;
    std::string content_type = "text/plain; charset=utf-8";
    std::string body;
  };
  using Handler = std::function<Response(const std::string& method, const std::string& path)>;

  HttpServer() = default;
  HttpServer(const HttpServer&) = delete;
  HttpServer& operator=(const HttpServer&) = delete;
  ~HttpServer();

  // Binds addr, given as "host:port", ":port" or "[v6addr]:port". Port 0
  // picks a free port; see Port().
  bool Listen(const std::string& addr, std::string* err);
  int Port() const { return port_; }
  // Serves until stop becomes true.
  void Serve(const Handler& handler, const std::atomic<bool>& stop);

 private:
  void HandleConnection(std::intptr_t fd, const Handler& handler);
  std::intptr_t fd_ = -1;  // a socket; SOCKET on Windows is pointer-sized
  int port_ = 0;
};

}  // namespace chipagent
