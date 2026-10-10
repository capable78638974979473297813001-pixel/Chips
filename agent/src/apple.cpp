// Apple silicon: the GPU shares one pool of unified memory with the CPU, so
// the useful numbers are total RAM and how much of it is in use.
#include "chipagent.hpp"

#if defined(__APPLE__) && defined(__aarch64__)
#include <mach/mach.h>
#include <sys/sysctl.h>

namespace chipagent {
namespace {

class AppleProvider : public Provider {
 public:
  std::string Name() const override { return "apple"; }

  std::vector<Device> Devices() override {
    Device d;
    d.vendor = "apple";
    d.source = "sysctl";
    d.unified_memory = true;
    char brand[128] = {};
    size_t len = sizeof brand;
    if (sysctlbyname("machdep.cpu.brand_string", brand, &len, nullptr, 0) == 0) {
      d.name = std::string(brand) + " GPU";
    } else {
      d.name = "Apple silicon GPU";
    }
    uint64_t mem = 0;
    len = sizeof mem;
    if (sysctlbyname("hw.memsize", &mem, &len, nullptr, 0) == 0) d.memory_total_bytes = mem;

    vm_statistics64_data_t vm{};
    mach_msg_type_number_t count = HOST_VM_INFO64_COUNT;
    if (host_statistics64(mach_host_self(), HOST_VM_INFO64, reinterpret_cast<host_info64_t>(&vm),
                          &count) == KERN_SUCCESS) {
      uint64_t pages = uint64_t(vm.active_count) + vm.wire_count + vm.compressor_page_count;
      d.memory_used_bytes = pages * vm_page_size;
    }
    return {d};
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
