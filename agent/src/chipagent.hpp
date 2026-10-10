// chipagent: a vendor-neutral view of the accelerators in one machine.
//
// Each vendor exposes its hardware differently: NVIDIA through the NVML
// library, AMD and Intel through Linux DRM sysfs, Apple through sysctl and
// Mach. chipagent reads all of them and reports one schema, so the router can
// treat an H100, an MI300X and an M2 Ultra as interchangeable capacity.
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

struct Device {
  std::string vendor;  // "nvidia", "amd", "intel", "apple", ...
  std::string name;    // marketing name when the driver reports one
  std::string id;      // stable identifier: NVML UUID or PCI bus address
  std::string source;  // where the data came from: nvml, sysfs, sysctl, simulated
  uint64_t memory_total_bytes = 0;  // 0 = unknown
  uint64_t memory_used_bytes = 0;
  // True when the accelerator shares system RAM (Apple silicon) instead of
  // having its own VRAM/HBM.
  bool unified_memory = false;
  std::optional<double> utilization_percent;
  std::optional<double> temperature_c;
  std::optional<double> power_watts;
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

// AMD and Intel GPUs from Linux DRM sysfs under root (normally "/").
// NVIDIA cards found there are reported too, with less detail, unless
// skip_nvidia is set because NVML already covers them.
std::unique_ptr<Provider> OpenSysfs(const std::string& root, bool skip_nvidia);

// The Apple silicon GPU and its unified memory. nullptr on other platforms.
std::unique_ptr<Provider> OpenApple();

// Fixed devices for demos and tests. Each spec is
// "vendor:name:memory_gib[:utilization_percent]".
std::unique_ptr<Provider> OpenSimulated(const std::vector<std::string>& specs, std::string* err);

// Devices from every provider, in provider order.
std::vector<Device> Collect(const std::vector<std::unique_ptr<Provider>>& providers);

// The JSON document served at /v1/telemetry.
std::string RenderTelemetry(const std::vector<Device>& devices, const std::string& hostname,
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
  void HandleConnection(int fd, const Handler& handler);
  int fd_ = -1;
  int port_ = 0;
};

}  // namespace chipagent
