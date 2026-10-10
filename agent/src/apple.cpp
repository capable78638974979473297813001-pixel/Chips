// macOS. Apple silicon GPUs share one pool of unified memory with the CPU and
// are not PCI devices, so they are described from sysctl. Intel Macs carry
// ordinary PCI GPUs (Intel, AMD, NVIDIA), found through the IOKit registry.
#include "chipagent.hpp"

#if defined(__APPLE__)
#include <CoreFoundation/CoreFoundation.h>
#include <cstdio>
#include <IOKit/IOKitLib.h>
#include <mach/mach.h>
#include <sys/sysctl.h>

namespace chipagent {
namespace {

// IOKit stores PCI IDs as little-endian bytes in CFData.
std::optional<unsigned> DataUint(io_registry_entry_t e, CFStringRef key) {
  CFTypeRef v = IORegistryEntryCreateCFProperty(e, key, kCFAllocatorDefault, 0);
  if (!v) return std::nullopt;
  std::optional<unsigned> out;
  if (CFGetTypeID(v) == CFDataGetTypeID()) {
    CFDataRef d = static_cast<CFDataRef>(v);
    const UInt8* b = CFDataGetBytePtr(d);
    CFIndex n = CFDataGetLength(d);
    unsigned x = 0;
    for (CFIndex i = 0; i < n && i < 4; ++i) x |= unsigned(b[i]) << (8 * i);
    out = x;
  }
  CFRelease(v);
  return out;
}

std::string DataString(io_registry_entry_t e, CFStringRef key) {
  CFTypeRef v = IORegistryEntryCreateCFProperty(e, key, kCFAllocatorDefault, 0);
  if (!v) return "";
  std::string out;
  if (CFGetTypeID(v) == CFDataGetTypeID()) {
    CFDataRef d = static_cast<CFDataRef>(v);
    out.assign(reinterpret_cast<const char*>(CFDataGetBytePtr(d)), static_cast<size_t>(CFDataGetLength(d)));
    out = out.substr(0, out.find('\0'));
  }
  CFRelease(v);
  return out;
}

std::optional<long long> NumberProperty(io_registry_entry_t e, CFStringRef key) {
  CFTypeRef v = IORegistryEntryCreateCFProperty(e, key, kCFAllocatorDefault, 0);
  if (!v) return std::nullopt;
  std::optional<long long> out;
  long long n = 0;
  if (CFGetTypeID(v) == CFNumberGetTypeID() && CFNumberGetValue(static_cast<CFNumberRef>(v), kCFNumberLongLongType, &n)) {
    out = n;
  }
  CFRelease(v);
  return out;
}

class AppleProvider : public Provider {
 public:
  std::string Name() const override { return "apple"; }

  std::vector<Device> Devices() override {
    std::vector<Device> out;
#if defined(__aarch64__)
    out.push_back(AppleSiliconGpu());
#endif
    for (auto& d : PciGpus()) out.push_back(std::move(d));
    return out;
  }

 private:
  static Device AppleSiliconGpu() {
    Device d;
    d.vendor = "apple";
    d.kind = "gpu";
    d.source = "sysctl";
    d.unified_memory = true;
    d.id = "apple-gpu";
    char brand[128] = {};
    size_t len = sizeof brand;
    d.name = sysctlbyname("machdep.cpu.brand_string", brand, &len, nullptr, 0) == 0
                 ? std::string(brand) + " GPU"
                 : "Apple silicon GPU";
    uint64_t mem = 0;
    len = sizeof mem;
    if (sysctlbyname("hw.memsize", &mem, &len, nullptr, 0) == 0) d.memory_total_bytes = mem;
    vm_statistics64_data_t vm{};
    mach_msg_type_number_t count = HOST_VM_INFO64_COUNT;
    if (host_statistics64(mach_host_self(), HOST_VM_INFO64, reinterpret_cast<host_info64_t>(&vm), &count) ==
        KERN_SUCCESS) {
      d.memory_used_bytes = (uint64_t(vm.active_count) + vm.wire_count + vm.compressor_page_count) * vm_page_size;
    }
    return d;
  }

  static std::vector<Device> PciGpus() {
    std::vector<Device> out;
    io_iterator_t it = 0;
    // MACH_PORT_NULL selects the default main port on every macOS version.
    if (IOServiceGetMatchingServices(MACH_PORT_NULL, IOServiceMatching("IOPCIDevice"), &it) != KERN_SUCCESS) {
      return out;
    }
    for (io_registry_entry_t e; (e = IOIteratorNext(it)) != 0; IOObjectRelease(e)) {
      auto vendor = DataUint(e, CFSTR("vendor-id"));
      auto cls = DataUint(e, CFSTR("class-code"));
      if (!vendor || !cls) continue;
      unsigned device = DataUint(e, CFSTR("device-id")).value_or(0);
      PciIdentity pid = ClassifyPci(*vendor, device, *cls);
      if (pid.vendor.empty()) continue;
      Device d;
      d.vendor = pid.vendor;
      d.kind = pid.kind;
      d.source = "iokit";
      std::string model = DataString(e, CFSTR("model"));
      d.name = model.empty() ? pid.name : model;
      char path[64];
      std::snprintf(path, sizeof path, "iokit-%llx", static_cast<unsigned long long>(e));
      d.id = path;
      if (auto mb = NumberProperty(e, CFSTR("VRAM,totalMB")); mb && *mb > 0) {
        d.memory_total_bytes = uint64_t(*mb) << 20;
      }
      out.push_back(std::move(d));
    }
    IOObjectRelease(it);
    return out;
  }
};

}  // namespace

std::unique_ptr<Provider> OpenApple() { return std::make_unique<AppleProvider>(); }

}  // namespace chipagent

#else

namespace chipagent {
std::unique_ptr<Provider> OpenApple() { return nullptr; }
}  // namespace chipagent

#endif
