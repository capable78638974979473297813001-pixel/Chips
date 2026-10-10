// NVIDIA support through NVML (libnvidia-ml), which ships with the driver.
// Only the stable C entry points are used, declared here so the build needs
// neither the CUDA toolkit nor nvml.h.
#include <dlfcn.h>

#include "chipagent.hpp"

namespace chipagent {
namespace {

using nvmlReturn = int;  // NVML_SUCCESS == 0
using nvmlDevice = struct nvmlDevice_st*;
struct nvmlMemory {
  unsigned long long total, free, used;
};
struct nvmlUtilization {
  unsigned int gpu, memory;
};
constexpr int kTemperatureGpu = 0;  // NVML_TEMPERATURE_GPU

class NvmlProvider : public Provider {
 public:
  explicit NvmlProvider(void* lib) : lib_(lib) {}
  ~NvmlProvider() override {
    if (initialized_) shutdown_();
    dlclose(lib_);
  }

  // Resolves the entry points and initializes NVML. Utilization, temperature,
  // power and UUID are optional: some GPUs and drivers do not support them.
  bool Init() {
    bool required = Sym(init_, "nvmlInit_v2") && Sym(shutdown_, "nvmlShutdown") &&
                    Sym(count_, "nvmlDeviceGetCount_v2") &&
                    Sym(handle_, "nvmlDeviceGetHandleByIndex_v2") &&
                    Sym(name_, "nvmlDeviceGetName") && Sym(memory_, "nvmlDeviceGetMemoryInfo");
    if (!required) return false;
    Sym(util_, "nvmlDeviceGetUtilizationRates");
    Sym(temp_, "nvmlDeviceGetTemperature");
    Sym(power_, "nvmlDeviceGetPowerUsage");
    Sym(uuid_, "nvmlDeviceGetUUID");
    initialized_ = init_() == 0;
    return initialized_;
  }

  std::string Name() const override { return "nvml"; }

  std::vector<Device> Devices() override {
    std::vector<Device> out;
    unsigned int n = 0;
    if (count_(&n) != 0) return out;
    for (unsigned int i = 0; i < n; ++i) {
      nvmlDevice h = nullptr;
      if (handle_(i, &h) != 0) continue;
      Device d;
      d.vendor = "nvidia";
      d.source = "nvml";
      char buf[96] = {};
      d.name = name_(h, buf, sizeof buf) == 0 ? buf : "NVIDIA GPU";
      if (uuid_ && uuid_(h, buf, sizeof buf) == 0) d.id = buf;
      nvmlMemory mem{};
      if (memory_(h, &mem) == 0) {
        d.memory_total_bytes = mem.total;
        d.memory_used_bytes = mem.used;
      }
      nvmlUtilization util{};
      if (util_ && util_(h, &util) == 0) d.utilization_percent = util.gpu;
      unsigned int v = 0;
      if (temp_ && temp_(h, kTemperatureGpu, &v) == 0) d.temperature_c = v;
      if (power_ && power_(h, &v) == 0) d.power_watts = v / 1000.0;  // reported in milliwatts
      out.push_back(std::move(d));
    }
    return out;
  }

 private:
  template <typename F>
  bool Sym(F& fn, const char* symbol) {
    fn = reinterpret_cast<F>(dlsym(lib_, symbol));
    return fn != nullptr;
  }

  void* lib_;
  bool initialized_ = false;
  nvmlReturn (*init_)() = nullptr;
  nvmlReturn (*shutdown_)() = nullptr;
  nvmlReturn (*count_)(unsigned int*) = nullptr;
  nvmlReturn (*handle_)(unsigned int, nvmlDevice*) = nullptr;
  nvmlReturn (*name_)(nvmlDevice, char*, unsigned int) = nullptr;
  nvmlReturn (*memory_)(nvmlDevice, nvmlMemory*) = nullptr;
  nvmlReturn (*util_)(nvmlDevice, nvmlUtilization*) = nullptr;
  nvmlReturn (*temp_)(nvmlDevice, int, unsigned int*) = nullptr;
  nvmlReturn (*power_)(nvmlDevice, unsigned int*) = nullptr;
  nvmlReturn (*uuid_)(nvmlDevice, char*, unsigned int) = nullptr;
};

}  // namespace

std::unique_ptr<Provider> OpenNvml(const std::string& library) {
  std::vector<std::string> candidates = {library};
  if (library.empty()) candidates = {"libnvidia-ml.so.1", "libnvidia-ml.so"};
  for (const auto& path : candidates) {
    void* lib = dlopen(path.c_str(), RTLD_NOW | RTLD_LOCAL);
    if (!lib) continue;
    auto p = std::make_unique<NvmlProvider>(lib);
    if (p->Init()) return p;
    return nullptr;  // found the library but the driver is unusable
  }
  return nullptr;
}

}  // namespace chipagent
