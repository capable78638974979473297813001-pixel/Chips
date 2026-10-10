// Windows: DXGI lists every GPU with a WDDM driver (NVIDIA, AMD, Intel,
// Qualcomm Adreno, Moore Threads, ...) with its PCI IDs and dedicated memory,
// on Windows 7 and later.
#include "chipagent.hpp"

#ifdef _WIN32
#include <dxgi.h>
#include <windows.h>

#include <cstdio>

namespace chipagent {
namespace {

constexpr unsigned kVendorNvidia = 0x10de;

std::string Utf8(const wchar_t* w) {
  int n = WideCharToMultiByte(CP_UTF8, 0, w, -1, nullptr, 0, nullptr, nullptr);
  if (n <= 1) return "";
  std::string s(static_cast<size_t>(n - 1), '\0');
  WideCharToMultiByte(CP_UTF8, 0, w, -1, &s[0], n, nullptr, nullptr);
  return s;
}

class DxgiProvider : public Provider {
 public:
  explicit DxgiProvider(bool skip_nvidia) : skip_nvidia_(skip_nvidia) {}

  std::string Name() const override { return "dxgi"; }

  std::vector<Device> Devices() override {
    std::vector<Device> out;
    IDXGIFactory1* factory = nullptr;
    if (FAILED(CreateDXGIFactory1(IID_IDXGIFactory1, reinterpret_cast<void**>(&factory)))) return out;
    IDXGIAdapter1* adapter = nullptr;
    for (UINT i = 0; factory->EnumAdapters1(i, &adapter) != DXGI_ERROR_NOT_FOUND; ++i) {
      DXGI_ADAPTER_DESC1 desc{};
      bool ok = SUCCEEDED(adapter->GetDesc1(&desc));
      adapter->Release();
      if (!ok || (desc.Flags & DXGI_ADAPTER_FLAG_SOFTWARE)) continue;  // Microsoft Basic Render Driver
      if (skip_nvidia_ && desc.VendorId == kVendorNvidia) continue;
      PciIdentity pid = ClassifyPci(desc.VendorId, desc.DeviceId, 0x030000);
      if (pid.vendor.empty()) continue;
      Device d;
      d.vendor = pid.vendor;
      d.kind = pid.kind;
      d.source = "dxgi";
      d.name = Utf8(desc.Description);
      if (d.name.empty()) d.name = pid.name;
      char luid[40];
      std::snprintf(luid, sizeof luid, "luid-%08lx-%08lx", static_cast<unsigned long>(desc.AdapterLuid.HighPart),
                    static_cast<unsigned long>(desc.AdapterLuid.LowPart));
      d.id = luid;
      d.memory_total_bytes = desc.DedicatedVideoMemory;
      out.push_back(std::move(d));
    }
    factory->Release();
    return out;
  }

 private:
  bool skip_nvidia_;
};

}  // namespace

std::unique_ptr<Provider> OpenDxgi(bool skip_nvidia) { return std::make_unique<DxgiProvider>(skip_nvidia); }

}  // namespace chipagent

#else

namespace chipagent {
std::unique_ptr<Provider> OpenDxgi(bool) { return nullptr; }
}  // namespace chipagent

#endif
