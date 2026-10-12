// NVIDIA support through NVML (libnvidia-ml on Linux, nvml.dll on Windows),
// which ships with the driver. Only the stable C entry points are used,
// declared here so the build needs neither the CUDA toolkit nor nvml.h.
#include "chipagent.hpp"
#include "dynlib.hpp"

namespace chipagent {
namespace {

using nvmlReturn = int;  // NVML_SUCCESS == 0
using nvmlDevice = struct nvmlDevice_st*;
struct nvmlMemory {
  unsigned long long total, free, used;
};
// nvmlMemory_v2_t (driver 510+) reports memory the driver reserves for
// itself separately. The v1 call counts it as used, so an idle GPU shows
// hundreds of MiB in use; v2's used matches nvidia-smi.
struct nvmlMemoryV2 {
  unsigned int version;
  unsigned long long total, reserved, free, used;
};
constexpr unsigned int kMemoryV2 = sizeof(nvmlMemoryV2) | (2u << 24);  // NVML_STRUCT_VERSION(Memory, 2)
struct nvmlUtilization {
  unsigned int gpu, memory;
};
constexpr int kTemperatureGpu = 0;  // NVML_TEMPERATURE_GPU
constexpr nvmlReturn kGpuIsLost = 15;  // NVML_ERROR_GPU_IS_LOST
constexpr const char* kLostMessage =
    "GPU is lost: it stopped responding to the driver (NVIDIA Xid 79, fallen off the bus); reset the GPU or reboot";

class NvmlProvider : public Provider {
 public:
  explicit NvmlProvider(void* lib) : lib_(lib) {}
  ~NvmlProvider() override {
    if (initialized_) shutdown_();
    CloseLibrary(lib_);
  }

  // Resolves the entry points and initializes NVML. Utilization, temperature,
  // power and UUID are optional: some GPUs and drivers do not support them.
  bool Init() {
    bool required = Sym(init_, "nvmlInit_v2") && Sym(shutdown_, "nvmlShutdown") &&
                    Sym(count_, "nvmlDeviceGetCount_v2") &&
                    Sym(handle_, "nvmlDeviceGetHandleByIndex_v2") &&
                    Sym(name_, "nvmlDeviceGetName") && Sym(memory_, "nvmlDeviceGetMemoryInfo");
    if (!required) return false;
    Sym(memory2_, "nvmlDeviceGetMemoryInfo_v2");
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
      Device d;
      d.vendor = "nvidia";
      d.kind = "gpu";
      d.source = "nvml";
      if (nvmlReturn rc = handle_(i, &h); rc != 0) {
        if (rc != kGpuIsLost) continue;
        // Still counted, so the machine shows a failed GPU instead of
        // quietly having one fewer.
        d.name = "NVIDIA GPU " + std::to_string(i);
        d.id = "nvml:" + std::to_string(i);
        d.error = kLostMessage;
        out.push_back(std::move(d));
        continue;
      }
      char buf[96] = {};
      d.name = name_(h, buf, sizeof buf) == 0 ? buf : "NVIDIA GPU";
      if (uuid_ && uuid_(h, buf, sizeof buf) == 0) d.id = buf;
      nvmlMemoryV2 mem2{};
      mem2.version = kMemoryV2;
      nvmlMemory mem{};
      nvmlReturn rc = memory2_ ? memory2_(h, &mem2) : -1;
      if (rc == 0) {
        d.memory_total_bytes = mem2.total;
        d.memory_used_bytes = mem2.used;
      } else if (rc != kGpuIsLost && (rc = memory_(h, &mem)) == 0) {
        d.memory_total_bytes = mem.total;
        d.memory_used_bytes = mem.used;
      }
      if (rc == kGpuIsLost) d.error = kLostMessage;
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
    fn = reinterpret_cast<F>(LibrarySymbol(lib_, symbol));
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
  nvmlReturn (*memory2_)(nvmlDevice, nvmlMemoryV2*) = nullptr;
  nvmlReturn (*util_)(nvmlDevice, nvmlUtilization*) = nullptr;
  nvmlReturn (*temp_)(nvmlDevice, int, unsigned int*) = nullptr;
  nvmlReturn (*power_)(nvmlDevice, unsigned int*) = nullptr;
  nvmlReturn (*uuid_)(nvmlDevice, char*, unsigned int) = nullptr;
};

}  // namespace

std::unique_ptr<Provider> OpenNvml(const std::string& library) {
  std::vector<std::string> candidates = {library};
#ifdef _WIN32
  if (library.empty()) {
    candidates = {"nvml.dll", "C:\\Program Files\\NVIDIA Corporation\\NVSMI\\nvml.dll"};
  }
#else
  if (library.empty()) candidates = {"libnvidia-ml.so.1", "libnvidia-ml.so"};
#endif
  for (const auto& path : candidates) {
    void* lib = OpenLibrary(path);
    if (!lib) continue;
    auto p = std::make_unique<NvmlProvider>(lib);
    if (p->Init()) return p;
    return nullptr;  // found the library but the driver is unusable
  }
  return nullptr;
}

}  // namespace chipagent
