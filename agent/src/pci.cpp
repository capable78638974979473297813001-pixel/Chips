// PCI-based detection. Every discrete GPU and AI accelerator made since PCI
// arrived in the 1990s announces a vendor ID and a class code, so this finds
// hardware chipagent has no special support for. Where a Linux driver
// publishes live readings in sysfs (amdgpu does), they are added.
#include <algorithm>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iterator>

#include "chipagent.hpp"

namespace fs = std::filesystem;

namespace chipagent {
namespace {

constexpr unsigned kVendorAmd = 0x1002;
constexpr unsigned kVendorNvidia = 0x10de;

struct Vendor {
  unsigned id;
  const char* vendor;
  const char* label;
  const char* display_kind;      // kind for PCI class 0x03 (display controller)
  const char* accelerator_kind;  // kind for class 0x12 (processing accelerator) / 0x0b40 (co-processor)
};

// Vendors chipagent knows by name. Display-only entries are server BMCs,
// virtual machine adapters and 1990s/2000s graphics chips that predate GPU
// compute; they are reported but never counted as AI capacity.
constexpr Vendor kVendors[] = {
    {0x10de, "nvidia", "NVIDIA GPU", "gpu", "gpu"},
    {0x1002, "amd", "AMD GPU", "gpu", "gpu"},
    {0x8086, "intel", "Intel GPU", "gpu", "npu"},
    {0x1da3, "habana", "Intel Gaudi (Habana) accelerator", "accelerator", "accelerator"},
    {0x1d0f, "aws", "AWS Inferentia/Trainium accelerator", "display", "accelerator"},
    {0x1ae0, "google", "Google TPU", "display", "accelerator"},
    {0x19e5, "huawei", "Huawei Ascend NPU", "display", "npu"},
    {0x17cb, "qualcomm", "Qualcomm Cloud AI accelerator", "gpu", "accelerator"},
    {0x1e52, "tenstorrent", "Tenstorrent accelerator", "accelerator", "accelerator"},
    {0x10ee, "xilinx", "AMD Xilinx FPGA/AI engine", "display", "accelerator"},
    {0x1172, "altera", "Intel Altera FPGA", "display", "accelerator"},
    {0x1a03, "aspeed", "ASPEED BMC graphics", "display", "display"},
    {0x102b, "matrox", "Matrox graphics", "display", "display"},
    {0x1234, "qemu", "QEMU virtual graphics", "display", "display"},
    {0x1b36, "redhat", "QXL virtual graphics", "display", "display"},
    {0x1af4, "virtio", "virtio-gpu", "display", "display"},
    {0x15ad, "vmware", "VMware SVGA", "display", "display"},
    {0x80ee, "virtualbox", "VirtualBox graphics", "display", "display"},
    {0x1414, "microsoft", "Hyper-V virtual graphics", "display", "display"},
    {0x1013, "cirrus", "Cirrus Logic graphics", "display", "display"},
    {0x121a, "3dfx", "3dfx Voodoo", "display", "display"},
    {0x5333, "s3", "S3 graphics", "display", "display"},
    {0x1039, "sis", "SiS graphics", "display", "display"},
    {0x1106, "via", "VIA graphics", "display", "display"},
    {0x1023, "trident", "Trident graphics", "display", "display"},
    {0x18ca, "xgi", "XGI graphics", "display", "display"},
    {0x1163, "rendition", "Rendition Verite", "display", "display"},
    {0x10c8, "neomagic", "NeoMagic graphics", "display", "display"},
    {0x12d2, "nvidia", "NVIDIA/SGS-Thomson Riva", "display", "display"},
};

// Device IDs of common data-center and workstation AMD parts, for drivers
// that do not publish product_name.
const char* AmdName(unsigned device) {
  switch (device) {
    case 0x74a1: return "AMD Instinct MI300X";
    case 0x740c: return "AMD Instinct MI250X/MI250";
    case 0x740f: return "AMD Instinct MI210";
    case 0x738c: return "AMD Instinct MI100";
    case 0x744c: return "AMD Radeon RX 7900 XTX/XT";
  }
  return nullptr;
}

std::string Hex4(unsigned v) {
  char buf[8];
  std::snprintf(buf, sizeof buf, "%04x", v & 0xffff);
  return buf;
}

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

std::string ReadAll(const fs::path& p) {
  std::ifstream f(p, std::ios::binary);
  return std::string(std::istreambuf_iterator<char>(f), std::istreambuf_iterator<char>());
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

class PciProvider : public Provider {
 public:
  PciProvider(fs::path root, bool skip_nvidia) : root_(std::move(root)), skip_nvidia_(skip_nvidia) {}

  std::string Name() const override { return "pci"; }

  std::vector<Device> Devices() override {
    std::vector<fs::path> devs;
    std::error_code ec;
    for (const auto& e : fs::directory_iterator(root_ / "sys/bus/pci/devices", ec)) devs.push_back(e.path());
    std::sort(devs.begin(), devs.end());  // by bus address, so order is stable

    std::vector<Device> out;
    for (const auto& dev : devs) {
      auto vendor = ReadUint(dev / "vendor", 16);
      auto cls = ReadUint(dev / "class", 16);
      if (!vendor || !cls) continue;
      unsigned device = static_cast<unsigned>(ReadUint(dev / "device", 16).value_or(0));
      PciIdentity pid = ClassifyPci(static_cast<unsigned>(*vendor), device, static_cast<unsigned>(*cls));
      if (pid.vendor.empty() || (skip_nvidia_ && *vendor == kVendorNvidia)) continue;

      Device d;
      d.vendor = pid.vendor;
      d.kind = pid.kind;
      d.name = pid.name;
      d.source = "sysfs";
      d.id = dev.filename().string();  // PCI address, e.g. 0000:03:00.0
      if (*vendor == kVendorAmd) {
        if (auto product = ReadLine(dev / "product_name"); product && !product->empty()) d.name = *product;
      }
      if (*vendor == kVendorNvidia) d.name += " (install the NVIDIA driver for NVML details)";
      d.memory_total_bytes = ReadUint(dev / "mem_info_vram_total").value_or(0);
      d.memory_used_bytes = ReadUint(dev / "mem_info_vram_used").value_or(0);
      if (auto busy = ReadUint(dev / "gpu_busy_percent")) d.utilization_percent = static_cast<double>(*busy);
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

uint64_t MemTotalBytes(const fs::path& meminfo) {
  std::ifstream f(meminfo);
  std::string key;
  uint64_t kb = 0;
  while (f >> key >> kb) {
    if (key == "MemTotal:") return kb * 1024;
    f.ignore(1 << 10, '\n');
  }
  return 0;
}

class DeviceTreeProvider : public Provider {
 public:
  explicit DeviceTreeProvider(fs::path root) : root_(std::move(root)) {}

  std::string Name() const override { return "devicetree"; }

  std::vector<Device> Devices() override {
    // "compatible" is a list of NUL-separated strings like "nvidia,p3737-0000+p3701-0005\0nvidia,tegra234\0".
    std::string compatible = ReadAll(root_ / "proc/device-tree/compatible");
    if (compatible.find("nvidia,tegra") == std::string::npos) return {};
    Device d;
    d.vendor = "nvidia";
    d.kind = "gpu";
    d.source = "devicetree";
    d.unified_memory = true;  // Jetson GPUs share system RAM
    std::string model = ReadAll(root_ / "proc/device-tree/model");
    model = model.substr(0, model.find('\0'));
    d.name = model.empty() ? "NVIDIA Jetson" : model;
    d.id = "tegra";
    d.memory_total_bytes = MemTotalBytes(root_ / "proc/meminfo");
    return {d};
  }

 private:
  fs::path root_;
};

}  // namespace

PciIdentity ClassifyPci(unsigned vendor, unsigned device, unsigned class_code) {
  unsigned base = class_code >> 16, sub = (class_code >> 8) & 0xff;
  bool display = base == 0x03;
  bool accelerator = base == 0x12 || (base == 0x0b && sub == 0x40);
  if (!display && !accelerator) return {};

  std::string ids = " [" + Hex4(vendor) + ":" + Hex4(device) + "]";
  for (const auto& v : kVendors) {
    if (v.id != vendor) continue;
    PciIdentity id{v.vendor, display ? v.display_kind : v.accelerator_kind, v.label + ids};
    if (vendor == kVendorAmd) {
      if (const char* n = AmdName(device)) id.name = n;
    }
    return id;
  }
  // Unknown vendor: still report it, so new chip makers show up without a code change.
  return {"pci-" + Hex4(vendor), display ? "gpu" : "accelerator", "PCI device" + ids};
}

std::unique_ptr<Provider> OpenPci(const std::string& root, bool skip_nvidia) {
  return std::make_unique<PciProvider>(root, skip_nvidia);
}

std::unique_ptr<Provider> OpenDeviceTree(const std::string& root) {
  return std::make_unique<DeviceTreeProvider>(root);
}

}  // namespace chipagent
