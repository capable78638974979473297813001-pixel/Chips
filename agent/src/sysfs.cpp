// AMD and Intel GPUs on Linux, read from DRM sysfs (/sys/class/drm/cardN).
// The amdgpu driver publishes VRAM size and usage, busy percentage and hwmon
// sensors there, so no ROCm libraries are needed.
#include <algorithm>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <fstream>

#include "chipagent.hpp"

namespace fs = std::filesystem;

namespace chipagent {
namespace {

constexpr unsigned kVendorAmd = 0x1002;
constexpr unsigned kVendorIntel = 0x8086;
constexpr unsigned kVendorNvidia = 0x10de;

std::optional<std::string> ReadLine(const fs::path& p) {
  std::ifstream f(p);
  std::string s;
  if (!f || !std::getline(f, s)) return std::nullopt;
  while (!s.empty() && (s.back() == '\r' || s.back() == ' ' || s.back() == '\0')) s.pop_back();
  return s;
}

std::optional<uint64_t> ReadUint(const fs::path& p, int base = 10) {
  auto s = ReadLine(p);
  if (!s || s->empty()) return std::nullopt;
  char* end = nullptr;
  unsigned long long v = std::strtoull(s->c_str(), &end, base);
  if (end == s->c_str()) return std::nullopt;
  return v;
}

// Device IDs of common data-center and workstation parts, for drivers that do
// not publish product_name.
std::string AmdName(unsigned device) {
  switch (device) {
    case 0x74a1: return "AMD Instinct MI300X";
    case 0x740c: return "AMD Instinct MI250X/MI250";
    case 0x740f: return "AMD Instinct MI210";
    case 0x738c: return "AMD Instinct MI100";
    case 0x744c: return "AMD Radeon RX 7900 XTX/XT";
  }
  return "";
}

std::string Hex4(unsigned v) {
  char buf[8];
  std::snprintf(buf, sizeof buf, "%04x", v & 0xffff);
  return buf;
}

// The first hwmon directory under dev, where temperature and power live.
std::optional<fs::path> Hwmon(const fs::path& dev) {
  std::error_code ec;
  std::vector<fs::path> dirs;
  for (const auto& e : fs::directory_iterator(dev / "hwmon", ec)) dirs.push_back(e.path());
  if (dirs.empty()) return std::nullopt;
  std::sort(dirs.begin(), dirs.end());
  return dirs.front();
}

bool IsCard(const std::string& n) {
  return n.size() > 4 && n.compare(0, 4, "card") == 0 &&
         std::all_of(n.begin() + 4, n.end(), [](char c) { return c >= '0' && c <= '9'; });
}

class SysfsProvider : public Provider {
 public:
  SysfsProvider(fs::path root, bool skip_nvidia) : root_(std::move(root)), skip_nvidia_(skip_nvidia) {}

  std::string Name() const override { return "sysfs"; }

  std::vector<Device> Devices() override {
    std::vector<std::pair<int, fs::path>> cards;
    std::error_code ec;
    for (const auto& e : fs::directory_iterator(root_ / "sys/class/drm", ec)) {
      std::string n = e.path().filename().string();
      if (IsCard(n)) cards.emplace_back(std::atoi(n.c_str() + 4), e.path());
    }
    std::sort(cards.begin(), cards.end());

    std::vector<Device> out;
    for (const auto& [num, card] : cards) {
      fs::path dev = card / "device";
      auto vendor = ReadUint(dev / "vendor", 16);
      auto device = ReadUint(dev / "device", 16).value_or(0);
      if (!vendor) continue;
      Device d;
      d.source = "sysfs";
      d.id = fs::canonical(dev, ec).filename().string();  // PCI address, e.g. 0000:03:00.0
      if (ec) d.id = card.filename().string();
      std::string pci = "[" + Hex4(*vendor) + ":" + Hex4(device) + "]";
      switch (*vendor) {
        case kVendorAmd:
          d.vendor = "amd";
          d.name = ReadLine(dev / "product_name").value_or("");
          if (d.name.empty()) d.name = AmdName(device);
          if (d.name.empty()) d.name = "AMD GPU " + pci;
          break;
        case kVendorIntel:
          d.vendor = "intel";
          d.name = "Intel GPU " + pci;
          break;
        case kVendorNvidia:
          if (skip_nvidia_) continue;
          d.vendor = "nvidia";
          d.name = "NVIDIA GPU " + pci + " (NVML unavailable)";
          break;
        default:
          continue;  // BMC display adapters and the like
      }
      d.memory_total_bytes = ReadUint(dev / "mem_info_vram_total").value_or(0);
      d.memory_used_bytes = ReadUint(dev / "mem_info_vram_used").value_or(0);
      if (auto busy = ReadUint(dev / "gpu_busy_percent")) d.utilization_percent = *busy;
      if (auto hw = Hwmon(dev)) {
        if (auto t = ReadUint(*hw / "temp1_input")) d.temperature_c = *t / 1000.0;  // millidegrees
        auto p = ReadUint(*hw / "power1_average");
        if (!p) p = ReadUint(*hw / "power1_input");
        if (p) d.power_watts = *p / 1e6;  // microwatts
      }
      out.push_back(std::move(d));
    }
    return out;
  }

 private:
  fs::path root_;
  bool skip_nvidia_;
};

}  // namespace

std::unique_ptr<Provider> OpenSysfs(const std::string& root, bool skip_nvidia) {
  return std::make_unique<SysfsProvider>(root, skip_nvidia);
}

}  // namespace chipagent
