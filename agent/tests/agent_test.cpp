// Unit tests for chipagent. No framework: each CHECK failure prints and the
// process exits non-zero at the end.
#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#define close closesocket
#else
#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <unistd.h>
#endif

#include <algorithm>
#include <chrono>
#include <cmath>
#include <csignal>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <sstream>
#include <thread>

#include "chipagent.hpp"
#include "json.hpp"

namespace fs = std::filesystem;
using chipagent::Device;

static int failures = 0;

#define CHECK(cond)                                                          \
  do {                                                                       \
    if (!(cond)) {                                                           \
      std::cerr << __FILE__ << ":" << __LINE__ << ": CHECK(" #cond ") failed\n"; \
      ++failures;                                                            \
    }                                                                        \
  } while (0)

#define CHECK_EQ(a, b)                                                                    \
  do {                                                                                    \
    auto va = (a);                                                                        \
    auto vb = (b);                                                                        \
    if (!(va == vb)) {                                                                    \
      std::cerr << __FILE__ << ":" << __LINE__ << ": " #a " == " #b " failed: " << va    \
                << " vs " << vb << "\n";                                                  \
      ++failures;                                                                         \
    }                                                                                     \
  } while (0)

#ifndef _WIN32  // helpers for the Linux /sys and /proc fixtures
static void Write(const fs::path& p, const std::string& content) {
  fs::create_directories(p.parent_path());
  std::ofstream(p, std::ios::binary) << content;
}

static fs::path TempDir(const std::string& tag) {
  auto stamp = std::chrono::steady_clock::now().time_since_epoch().count();
  fs::path p = fs::temp_directory_path() / ("chipagent-" + tag + "-" + std::to_string(stamp));
  fs::remove_all(p);
  return p;
}

#endif

static std::string ReadFile(const fs::path& p) {
  std::ifstream f(p);
  std::stringstream ss;
  ss << f.rdbuf();
  return ss.str();
}

static void TestJsonEscape() {
  CHECK_EQ(chipagent::JsonEscape("plain"), std::string("plain"));
  CHECK_EQ(chipagent::JsonEscape("a\"b\\c\nd\x01"), std::string("a\\\"b\\\\c\\nd\\u0001"));
}

static void TestSimulated() {
  std::string err;
  auto p = chipagent::OpenSimulated({"nvidia:NVIDIA H100 80GB HBM3:80:35", "apple:Apple M2 Ultra GPU:192"}, &err);
  CHECK(p != nullptr);
  auto ds = p->Devices();
  CHECK_EQ(ds.size(), size_t(2));
  CHECK_EQ(ds[0].memory_total_bytes, uint64_t(80) << 30);
  CHECK(ds[0].utilization_percent && *ds[0].utilization_percent == 35);
  CHECK(!ds[0].unified_memory);
  CHECK(ds[1].unified_memory);
  CHECK(!ds[1].utilization_percent);
  CHECK_EQ(ds[0].kind, std::string("gpu"));

  auto tpu = chipagent::OpenSimulated({"google:TPU v5e:16::accelerator"}, &err);
  CHECK(tpu != nullptr);
  if (tpu) {
    auto t = tpu->Devices();
    CHECK_EQ(t[0].kind, std::string("accelerator"));
    CHECK(!t[0].utilization_percent);
  }

  for (const char* bad : {"nvidia", "nvidia:H100", ":H100:80", "nvidia:H100:lots", "nvidia:H100:-1",
                          "nvidia:H100:80:1:2", "nvidia:H100:80:1:gpu:x", "google:TPU:16::quantum"}) {
    err.clear();
    CHECK(chipagent::OpenSimulated({bad}, &err) == nullptr);
    CHECK(!err.empty());
  }
}

// The same file is decoded by the router's Go tests, which keeps the two
// sides of the schema in agreement.
static void TestGoldenTelemetry() {
  std::string err;
  std::vector<std::unique_ptr<chipagent::Provider>> providers;
  providers.push_back(chipagent::OpenSimulated(
      {"nvidia:NVIDIA H100 80GB HBM3:80:35", "amd:AMD Instinct MI300X:192"}, &err));
  auto devices = chipagent::Collect(providers);
  devices[1].memory_used_bytes = 1073741824;
  devices[1].temperature_c = 48.5;
  devices[1].power_watts = 301.25;
  chipagent::Host host;
  host.hostname = "gpu-node-1";
  host.os = "linux";
  host.arch = "x86_64";
  host.cpu = "AMD EPYC 9654 96-Core Processor";
  host.logical_cpus = 192;
  host.memory_total_bytes = 1649267441664;
  host.memory_used_bytes = 68719476736;
  host.cpu_features = {"avx2", "avx512f", "avx512_bf16"};
  devices.push_back(chipagent::Device{});
  devices[2].vendor = "aspeed";
  devices[2].kind = "display";
  devices[2].name = "ASPEED BMC graphics [1a03:2000]";
  devices[2].id = "0000:02:00.0";
  devices[2].source = "sysfs";
  std::string got = chipagent::RenderTelemetry(devices, host, "0.2.0");
  std::string want = ReadFile(fs::path(TESTDATA_DIR) / "telemetry.json");
  CHECK_EQ(got, want);
}

static void TestClassifyPci() {
  auto c = chipagent::ClassifyPci;
  CHECK_EQ(c(0x10de, 0x2330, 0x030200).kind, std::string("gpu"));
  CHECK_EQ(c(0x1002, 0x74a1, 0x120000).name, std::string("AMD Instinct MI300X"));
  CHECK_EQ(c(0x8086, 0x7d1d, 0x120000).kind, std::string("npu"));            // Intel NPU
  CHECK_EQ(c(0x1da3, 0x1020, 0x120000).vendor, std::string("habana"));       // Gaudi
  CHECK_EQ(c(0x1d0f, 0x7064, 0x0b4000).kind, std::string("accelerator"));    // Inferentia as co-processor
  CHECK_EQ(c(0x1d0f, 0x1111, 0x030000).kind, std::string("display"));        // EC2 VGA
  CHECK_EQ(c(0x19e5, 0xd801, 0x120000).kind, std::string("npu"));            // Ascend
  CHECK_EQ(c(0x19e5, 0x1711, 0x030000).kind, std::string("display"));        // Huawei BMC
  CHECK_EQ(c(0x121a, 0x0001, 0x030000).kind, std::string("display"));        // 3dfx Voodoo, 1996
  CHECK_EQ(c(0x12d2, 0x0018, 0x030000).kind, std::string("display"));        // Riva 128, 1997
  CHECK_EQ(c(0x1ed5, 0x0101, 0x030200).vendor, std::string("pci-1ed5"));     // unknown maker
  CHECK_EQ(c(0x1ed5, 0x0101, 0x030200).kind, std::string("gpu"));
  CHECK(c(0x8086, 0x1572, 0x020000).vendor.empty());                         // a NIC
}

#ifndef _WIN32  // PCI addresses contain ':', which Windows filenames cannot
static void TestPci() {
  fs::path root = TempDir("pci");
  fs::path pci = root / "sys/bus/pci/devices";
  auto dev = [&](const std::string& bus, const char* vendor, const char* device, const char* cls) {
    Write(pci / bus / "vendor", std::string(vendor) + "\n");
    Write(pci / bus / "device", std::string(device) + "\n");
    Write(pci / bus / "class", std::string(cls) + "\n");
    return pci / bus;
  };
  fs::path amd = dev("0000:03:00.0", "0x1002", "0x74a1", "0x120000");
  Write(amd / "mem_info_vram_total", "206141652992\n");
  Write(amd / "mem_info_vram_used", "1073741824\n");
  Write(amd / "gpu_busy_percent", "42\n");
  Write(amd / "hwmon/hwmon3/temp1_input", "51000\n");
  Write(amd / "hwmon/hwmon3/power1_average", "350000000\n");
  dev("0000:00:02.0", "0x8086", "0x56a0", "0x030000");  // Intel Arc
  dev("0000:41:00.0", "0x10de", "0x2330", "0x030200");  // H100
  dev("0000:02:00.0", "0x1a03", "0x2000", "0x030000");  // ASPEED BMC
  dev("0000:51:00.0", "0x1da3", "0x1020", "0x120000");  // Gaudi
  dev("0000:00:1f.6", "0x8086", "0x15bc", "0x020000");  // Ethernet: ignored
  Write(pci / "0000:00:00.0" / "vendor", "0x8086\n");  // no class file: ignored

  auto ds = chipagent::OpenPci(root.string(), true)->Devices();
  CHECK_EQ(ds.size(), size_t(4));
  if (ds.size() == 4) {
    CHECK_EQ(ds[0].vendor, std::string("intel"));
    CHECK_EQ(ds[0].name, std::string("Intel GPU [8086:56a0]"));
    CHECK_EQ(ds[1].kind, std::string("display"));
    CHECK_EQ(ds[2].vendor, std::string("amd"));
    CHECK_EQ(ds[2].name, std::string("AMD Instinct MI300X"));
    CHECK_EQ(ds[2].id, std::string("0000:03:00.0"));
    CHECK_EQ(ds[2].memory_total_bytes, uint64_t(206141652992));
    CHECK_EQ(ds[2].memory_used_bytes, uint64_t(1073741824));
    CHECK(ds[2].utilization_percent && *ds[2].utilization_percent == 42);
    CHECK(ds[2].temperature_c && *ds[2].temperature_c == 51);
    CHECK(ds[2].power_watts && *ds[2].power_watts == 350);
    CHECK_EQ(ds[3].vendor, std::string("habana"));
    CHECK_EQ(ds[3].kind, std::string("accelerator"));
  }

  // product_name wins over the built-in ID table; NVIDIA is listed when NVML is absent.
  Write(amd / "product_name", "AMD Instinct MI300X OAM\n");
  ds = chipagent::OpenPci(root.string(), false)->Devices();
  CHECK_EQ(ds.size(), size_t(5));
  if (ds.size() == 5) {
    CHECK_EQ(ds[2].name, std::string("AMD Instinct MI300X OAM"));
    CHECK_EQ(ds[3].vendor, std::string("nvidia"));
    CHECK_EQ(ds[3].source, std::string("sysfs"));
  }
  CHECK(chipagent::OpenPci((root / "missing").string(), true)->Devices().empty());
  fs::remove_all(root);
}

static void TestDeviceTree() {
  fs::path root = TempDir("dt");
  CHECK(chipagent::OpenDeviceTree(root.string())->Devices().empty());
  Write(root / "proc/device-tree/compatible", std::string("nvidia,p3737-0000+p3701-0005\0nvidia,tegra234\0", 46));
  Write(root / "proc/device-tree/model", std::string("NVIDIA Jetson AGX Orin Developer Kit\0", 37));
  Write(root / "proc/meminfo", "MemTotal:       65536000 kB\nMemFree: 1 kB\n");
  auto ds = chipagent::OpenDeviceTree(root.string())->Devices();
  CHECK_EQ(ds.size(), size_t(1));
  if (!ds.empty()) {
    CHECK_EQ(ds[0].name, std::string("NVIDIA Jetson AGX Orin Developer Kit"));
    CHECK(ds[0].unified_memory);
    CHECK_EQ(ds[0].memory_total_bytes, uint64_t(65536000) * 1024);
  }
  fs::remove_all(root);
}

#endif

#if !defined(_WIN32) && !defined(__APPLE__)  // ReadHost reads /proc only here; macOS uses sysctl
static bool Has(const std::vector<std::string>& v, const std::string& x) {
  return std::find(v.begin(), v.end(), x) != v.end();
}

static void TestReadHostFromProc() {
  fs::path root = TempDir("host");
  Write(root / "proc/cpuinfo", "processor\t: 0\nmodel name\t: Test CPU\nflags\t\t: fpu avx2\n\n");
  Write(root / "proc/meminfo", "MemTotal:       1000 kB\nMemFree:          10 kB\nMemAvailable:     400 kB\n");
  chipagent::Host h = chipagent::ReadHost(root.string());
  CHECK_EQ(h.cpu, std::string("Test CPU"));
  CHECK_EQ(h.memory_total_bytes, uint64_t(1000 * 1024));
  CHECK_EQ(h.memory_used_bytes, uint64_t(600 * 1024));
  CHECK(Has(h.cpu_features, "avx2"));
  fs::remove_all(root);
}
#endif

// Real /proc/cpuinfo excerpts; every architecture names its CPU differently.
static void TestParseCpuinfo() {
  struct Case {
    const char* arch;
    const char* text;
    const char* name;
    std::vector<std::string> features;
  };
  const Case cases[] = {
      {"x86_64",
       "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) Platinum 8480+\n"
       "flags\t\t: fpu sse2 avx avx2 fma avx512f avx512_bf16 amx_tile amx_bf16 hypervisor\n\n"
       "processor\t: 1\nmodel name\t: Other\n",
       "Intel(R) Xeon(R) Platinum 8480+", {"sse2", "avx", "avx2", "fma", "avx512f", "avx512_bf16", "amx_tile", "amx_bf16"}},
      {"i686 (1999)",
       "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Pentium III (Coppermine)\nflags\t\t: fpu mmx sse\n",
       "Pentium III (Coppermine)", {}},
      {"aarch64",
       "processor\t: 0\nBogoMIPS\t: 243.75\nFeatures\t: fp asimd evtstrm aes asimddp sve i8mm bf16\n"
       "CPU implementer\t: 0x41\nCPU architecture: 8\nCPU variant\t: 0x1\nCPU part\t: 0xd40\n\n",
       "Arm Neoverse V1", {"asimd", "asimddp", "sve", "i8mm", "bf16"}},
      {"aarch64 unknown part",
       "processor\t: 0\nFeatures\t: fp asimd\nCPU implementer\t: 0xc0\nCPU part\t: 0xac3\n",
       "Ampere CPU part 0xac3", {"asimd"}},
      {"armv7 (old kernel)",
       "Processor\t: ARMv7 Processor rev 4 (v7l)\nprocessor\t: 0\nBogoMIPS\t: 38.40\n\n"
       "Features\t: half thumb fastmult vfp edsp neon vfpv3 tls vfpv4\nHardware\t: BCM2709\n",
       "ARMv7 Processor rev 4 (v7l)", {"neon", "vfpv4"}},
      {"ppc64le",
       "processor\t: 0\ncpu\t\t: POWER9 (raw), altivec supported\nclock\t\t: 3800.000000MHz\n\n",
       "POWER9 (raw), altivec supported", {"altivec"}},
      {"riscv64",
       "processor\t: 0\nhart\t\t: 0\nisa\t\t: rv64imafdcv_zicsr_zifencei\nmmu\t\t: sv39\n"
       "uarch\t\t: thead,c908\n\n",
       "thead,c908", {"rvv"}},
      {"riscv64 no uarch", "processor\t: 0\nhart\t\t: 1\nisa\t\t: rv64imafdc\n", "RISC-V rv64imafdc", {}},
      {"s390x",
       "vendor_id       : IBM/S390\n# processors    : 4\nfeatures\t: esan3 zarch stfle msa vx vxd vxe gs\n"
       "processor 0: version = FF,  identification = 0B9A08,  machine = 8561\n",
       "IBM Z (machine 8561)", {"vx", "vxe"}},
      {"mips", "system type\t\t: MediaTek MT7621\nprocessor\t\t: 0\ncpu model\t\t: MIPS 1004Kc V2.15\n",
       "MIPS 1004Kc V2.15", {}},
      {"loongarch64", "system type\t\t: generic-loongson-machine\n\nprocessor\t\t: 0\nModel Name\t\t: Loongson-3A5000\n"
       "Features\t\t: cpucfg lam ual fpu lsx lasx\n",
       "Loongson-3A5000", {"lsx", "lasx"}},
      {"sparc64", "cpu\t\t: UltraSparc T2 (Niagara2)\nfpu\t\t: UltraSparc T2 integrated FPU\n",
       "UltraSparc T2 (Niagara2)", {}},
  };
  for (const auto& c : cases) {
    std::string name;
    std::vector<std::string> features;
    chipagent::ParseCpuinfo(c.text, &name, &features);
    if (name != c.name || features != c.features) {
      std::cerr << "cpuinfo " << c.arch << ": got \"" << name << "\" [";
      for (const auto& f : features) std::cerr << f << " ";
      std::cerr << "]\n";
      ++failures;
    }
  }
}

static void TestReadHostLive() {
  chipagent::Host h = chipagent::ReadHost("/");
  CHECK(!h.os.empty());
  CHECK(!h.arch.empty());
  CHECK(h.logical_cpus > 0);
  CHECK(h.memory_total_bytes > 0);
  std::cout << "this machine: " << h.os << "/" << h.arch << " \"" << h.cpu << "\", " << h.logical_cpus
            << " CPUs\n";
}

static void TestNvml() {
  CHECK(chipagent::OpenNvml("/nonexistent/libnvidia-ml.so.1") == nullptr);

  auto p = chipagent::OpenNvml(FAKE_NVML_PATH);
  CHECK(p != nullptr);
  if (!p) return;
  auto ds = p->Devices();
  CHECK_EQ(ds.size(), size_t(2));
  if (ds.size() != 2) return;
  CHECK_EQ(ds[0].vendor, std::string("nvidia"));
  CHECK_EQ(ds[0].name, std::string("NVIDIA H100 80GB HBM3"));
  CHECK_EQ(ds[0].id, std::string("GPU-aaaa"));
  CHECK_EQ(ds[0].memory_total_bytes, uint64_t(85899345920));
  CHECK_EQ(ds[0].memory_used_bytes, uint64_t(42949672960));
  CHECK(ds[0].utilization_percent && *ds[0].utilization_percent == 87);
  CHECK(ds[0].temperature_c && *ds[0].temperature_c == 61);
  CHECK(ds[0].power_watts && std::fabs(*ds[0].power_watts - 512.5) < 1e-9);
  CHECK_EQ(ds[1].name, std::string("NVIDIA L4"));
  CHECK(!ds[1].power_watts);  // the fake reports NOT_SUPPORTED
}

static void TestJson() {
  std::string err;
  auto j = chipagent::ParseJson(R"( {"a": [1, 2.5e1, -3], "s": "x\"y\u00e9\n", "t": true, "n": null, "o": {}} )", &err);
  CHECK(j.has_value());
  if (j) {
    CHECK_EQ(j->Get("a")->array.size(), size_t(3));
    CHECK(j->Get("a")->array[1].number == 25);
    CHECK_EQ(j->Str("s"), std::string("x\"y\xc3\xa9\n"));
    CHECK(j->Get("t")->boolean);
    CHECK(j->Get("n")->type == chipagent::Json::Null);
    CHECK(!j->Num("s").has_value());
  }
  for (const char* bad : {"", "{", "[1,]", "{\"a\" 1}", "\"unterminated", "{} x", "tru", "[1 2]"}) {
    CHECK(!chipagent::ParseJson(bad, &err).has_value());
  }
  CHECK(!chipagent::ParseJson(std::string(100, '[') + std::string(100, ']'), &err).has_value());  // too deep
}

#ifndef _WIN32
static void TestPlugins() {
  fs::path dir = TempDir("plugin");
  auto script = [&](const std::string& name, const std::string& body) {
    fs::path p = dir / name;
    Write(p, "#!/bin/sh\n" + body);
    fs::permissions(p, fs::perms::owner_all);
    return p.string();
  };

  auto ds = chipagent::OpenPlugin(script("ok.sh", R"(cat <<'J'
{"devices": [
  {"vendor": "google", "kind": "accelerator", "name": "TPU v5e", "id": "tpu0",
   "memory_total_bytes": 17179869184, "utilization_percent": 12.5, "temperature_c": null},
  {"vendor": "acme", "name": "Mystery NPU", "kind": "quantum"},
  {"name": "no vendor"}
]}
J
)"), 2000)->Devices();
  CHECK_EQ(ds.size(), size_t(2));
  if (ds.size() == 2) {
    CHECK_EQ(ds[0].name, std::string("TPU v5e"));
    CHECK_EQ(ds[0].source, std::string("plugin"));
    CHECK_EQ(ds[0].memory_total_bytes, uint64_t(17179869184));
    CHECK(ds[0].utilization_percent && *ds[0].utilization_percent == 12.5);
    CHECK(!ds[0].temperature_c);
    CHECK_EQ(ds[1].kind, std::string("accelerator"));  // unknown kinds fall back
  }
  CHECK_EQ(chipagent::OpenPlugin(script("array.sh", "echo '[{\"vendor\":\"x\",\"name\":\"y\"}]'\n"), 2000)->Devices().size(), size_t(1));
  CHECK(chipagent::OpenPlugin(script("bad.sh", "echo 'not json'\n"), 2000)->Devices().empty());
  CHECK(chipagent::OpenPlugin(script("fail.sh", "echo '{\"devices\":[]}'; exit 3\n"), 2000)->Devices().empty());

  // A hung tool is killed at the timeout, its children too.
  auto start = std::chrono::steady_clock::now();
  CHECK(chipagent::OpenPlugin(script("hang.sh", "sleep 30 & sleep 30\n"), 300)->Devices().empty());
  CHECK(std::chrono::steady_clock::now() - start < std::chrono::seconds(5));

  // The bundled plugins, run against sample output of the vendor tools.
  fs::path bin = dir / "bin";
  fs::create_directories(bin);
  Write(bin / "hl-smi", "#!/bin/sh\nprintf '0, HL-225, 0000:19:00.0, 98304, 512, 37, 41, 180\\n1, HL-225, 0000:1a:00.0, 98304, 0, N/A, 39, 95\\n'\n");
  Write(bin / "neuron-ls", "#!/bin/sh\necho '[{\"neuron_device\":0,\"bdf\":\"00:1e.0\",\"nc_count\":2,\"memory_size\":34359738368}]'\n");
  fs::permissions(bin / "hl-smi", fs::perms::owner_all);
  fs::permissions(bin / "neuron-ls", fs::perms::owner_all);
  std::string path = "PATH=" + bin.string() + ":$PATH ";
  ds = chipagent::OpenPlugin(path + PLUGINS_DIR "/gaudi-hl-smi.sh", 5000)->Devices();
  CHECK_EQ(ds.size(), size_t(2));
  if (ds.size() == 2) {
    CHECK_EQ(ds[0].vendor, std::string("habana"));
    CHECK_EQ(ds[0].name, std::string("Intel Gaudi HL-225"));
    CHECK_EQ(ds[0].id, std::string("0000:19:00.0"));
    CHECK_EQ(ds[0].memory_total_bytes, uint64_t(98304) << 20);
    CHECK(ds[0].utilization_percent && *ds[0].utilization_percent == 37);
    CHECK(ds[0].power_watts && *ds[0].power_watts == 180);
    CHECK(!ds[1].utilization_percent);  // "N/A"
  }
  if (std::system("command -v python3 > /dev/null") == 0) {
    ds = chipagent::OpenPlugin(path + PLUGINS_DIR "/aws-neuron.py", 5000)->Devices();
    CHECK_EQ(ds.size(), size_t(1));
    if (!ds.empty()) {
      CHECK_EQ(ds[0].name, std::string("AWS Neuron device 0 (2 NeuronCores)"));
      CHECK_EQ(ds[0].memory_total_bytes, uint64_t(34359738368));
    }

    // Intel GPUs: xpu-smi's JSON, one physical GPU and its SR-IOV virtual function.
    Write(bin / "xpu-smi", R"(#!/bin/sh
case "$*" in
  "discovery -j") echo '{"device_list":[{"device_id":0,"device_name":"Intel(R) Data Center GPU Max 1550","device_type":"GPU","device_function_type":"physical","pci_bdf_address":"0000:29:00.0"},{"device_id":1,"device_name":"vf","device_function_type":"virtual","pci_bdf_address":"0000:29:00.1"}]}' ;;
  "discovery -d 0 -j") echo '{"device_id":0,"memory_physical_size_byte":"137438953472"}' ;;
  "stats -d 0 -j") echo '{"device_id":0,"device_level":[{"metrics_type":"XPUM_STATS_GPU_UTILIZATION","value":42},{"metrics_type":"XPUM_STATS_MEMORY_USED","value":2048},{"metrics_type":"XPUM_STATS_POWER","value":300.5},{"metrics_type":"XPUM_STATS_GPU_CORE_TEMPERATURE","value":55}]}' ;;
  *) exit 2 ;;
esac
)");
    fs::permissions(bin / "xpu-smi", fs::perms::owner_all);
    ds = chipagent::OpenPlugin(path + PLUGINS_DIR "/intel-xpu-smi.py", 5000)->Devices();
    CHECK_EQ(ds.size(), size_t(1));
    if (!ds.empty()) {
      CHECK_EQ(ds[0].vendor, std::string("intel"));
      CHECK_EQ(ds[0].name, std::string("Intel(R) Data Center GPU Max 1550"));
      CHECK_EQ(ds[0].id, std::string("0000:29:00.0"));
      CHECK_EQ(ds[0].memory_total_bytes, uint64_t(128) << 30);
      CHECK_EQ(ds[0].memory_used_bytes, uint64_t(2048) << 20);
      CHECK(ds[0].utilization_percent && *ds[0].utilization_percent == 42);
      CHECK(ds[0].power_watts && *ds[0].power_watts == 300.5);
      CHECK(ds[0].temperature_c && *ds[0].temperature_c == 55);
    }

    // Google TPUs: chips from a fake PCI tree, usage from a stand-in for
    // the tpu-info package (two v5e chips).
    fs::path root = dir / "root";
    fs::path sys = root / "sys";
    const std::vector<std::pair<std::string, std::string>> tpus = {
        {"0000:00:04.0", "0x0063"}, {"0000:00:05.0", "0x0063"}, {"0000:00:06.0", "0x1234"}};
    for (const auto& [addr, device] : tpus) {
      fs::path d = sys / "bus/pci/devices" / addr;
      fs::create_directories(d);
      Write(d / "vendor", "0x1ae0\n");
      Write(d / "device", device + "\n");
      Write(d / "subsystem_device", "0x0000\n");
    }
    fs::path py = dir / "py" / "tpu_info";
    fs::create_directories(py);
    Write(py / "__init__.py", "");
    Write(py / "device.py", "def get_local_chips():\n    return ('v5e', 2)\n");
    Write(py / "metrics.py", R"(import collections
Usage = collections.namedtuple("Usage", "device_id memory_usage total_memory duty_cycle_pct")
def get_chip_usage(chip_type):
    return [Usage(1, 4 << 30, 16 << 30, 10.0), Usage(0, 8 << 30, 16 << 30, 75.5)]
)");
    std::string tpu = "CHIPAGENT_SYSFS_ROOT=" + root.string() + " PYTHONPATH=" + (dir / "py").string() + " " PLUGINS_DIR "/google-tpu.py";
    ds = chipagent::OpenPlugin(tpu, 5000)->Devices();
    CHECK_EQ(ds.size(), size_t(2));  // the unknown Google device is not a TPU chip
    if (ds.size() == 2) {
      CHECK_EQ(ds[0].vendor, std::string("google"));
      CHECK_EQ(ds[0].name, std::string("Google TPU v5e"));
      CHECK_EQ(ds[0].id, std::string("0000:00:04.0"));
      CHECK_EQ(ds[0].memory_total_bytes, uint64_t(16) << 30);
      CHECK_EQ(ds[0].memory_used_bytes, uint64_t(8) << 30);  // device 0, after sorting by id
      CHECK(ds[0].utilization_percent && *ds[0].utilization_percent == 75.5);
      CHECK(ds[1].utilization_percent && *ds[1].utilization_percent == 10.0);
    }
    // Without tpu-info (or a running workload) the chips are still listed.
    ds = chipagent::OpenPlugin("CHIPAGENT_SYSFS_ROOT=" + root.string() + " " PLUGINS_DIR "/google-tpu.py", 5000)->Devices();
    CHECK_EQ(ds.size(), size_t(2));
    if (!ds.empty()) {
      CHECK_EQ(ds[0].memory_total_bytes, uint64_t(16) << 30);
      CHECK(!ds[0].utilization_percent);
    }
  }
  fs::remove_all(dir);
}
#else
static void TestPlugins() {
  fs::path f = fs::temp_directory_path() / "chipagent-plugin.json";
  std::ofstream(f) << R"({"devices":[{"vendor":"x","name":"Windows plugin device"}]})";
  auto ds = chipagent::OpenPlugin("type \"" + f.string() + "\"", 5000)->Devices();
  CHECK_EQ(ds.size(), size_t(1));
  fs::remove(f);
}
#endif

static std::string HttpGet(int port, const std::string& request) {
  auto fd = socket(AF_INET, SOCK_STREAM, 0);
  sockaddr_in addr{};
  addr.sin_family = AF_INET;
  addr.sin_port = htons(static_cast<uint16_t>(port));
  addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  if (connect(fd, reinterpret_cast<sockaddr*>(&addr), sizeof addr) != 0) {
    close(fd);
    return "";
  }
  send(fd, request.data(), static_cast<int>(request.size()), 0);
  std::string out;
  char buf[4096];
  for (int n; (n = static_cast<int>(recv(fd, buf, sizeof buf, 0))) > 0;) out.append(buf, static_cast<size_t>(n));
  close(fd);
  return out;
}

static void TestHttp() {
  chipagent::HttpServer server;
  std::string err;
  CHECK(server.Listen("127.0.0.1:0", &err));
  CHECK(server.Port() > 0);
  std::atomic<bool> stop{false};
  std::thread t([&] {
    server.Serve(
        [](const std::string& method, const std::string& path) -> chipagent::HttpServer::Response {
          if (method != "GET") return {405, "text/plain", "no\n"};
          if (path == "/v1/telemetry") return {200, "application/json", "{\"devices\":[]}\n"};
          return {404, "text/plain", "not found\n"};
        },
        stop);
  });

  std::string r = HttpGet(server.Port(), "GET /v1/telemetry?x=1 HTTP/1.1\r\nHost: a\r\n\r\n");
  CHECK(r.rfind("HTTP/1.1 200 OK\r\n", 0) == 0);
  CHECK(r.find("Content-Type: application/json\r\n") != std::string::npos);
  CHECK(r.find("Content-Length: 15\r\n") != std::string::npos);
  CHECK(r.size() > 15 && r.compare(r.size() - 15, 15, "{\"devices\":[]}\n") == 0);
  CHECK(HttpGet(server.Port(), "GET /nope HTTP/1.1\r\n\r\n").rfind("HTTP/1.1 404", 0) == 0);
  CHECK(HttpGet(server.Port(), "POST /v1/telemetry HTTP/1.1\r\n\r\n").rfind("HTTP/1.1 405", 0) == 0);
  CHECK(HttpGet(server.Port(), "garbage\r\n\r\n").rfind("HTTP/1.1 400", 0) == 0);
  CHECK(HttpGet(server.Port(), std::string(9000, 'x')).rfind("HTTP/1.1 431", 0) == 0);

  stop = true;
  t.join();

  chipagent::HttpServer bad;
  CHECK(!bad.Listen("no-port", &err));
}

// FixedProvider returns the devices it was built with.
class FixedProvider : public chipagent::Provider {
 public:
  explicit FixedProvider(std::vector<chipagent::Device> d) : devices_(std::move(d)) {}
  std::string Name() const override { return "fixed"; }
  std::vector<chipagent::Device> Devices() override { return devices_; }

 private:
  std::vector<chipagent::Device> devices_;
};

static void TestCollectMerges() {
  CHECK_EQ(chipagent::DeviceKey("0000:3B:00.0"), std::string("0000:3b:00.0"));
  CHECK_EQ(chipagent::DeviceKey("3b:00.0"), std::string("0000:3b:00.0"));
  CHECK_EQ(chipagent::DeviceKey("00:1e.0"), std::string("0000:00:1e.0"));
  CHECK_EQ(chipagent::DeviceKey("00000000:3B:00.0"), std::string("0000:3b:00.0"));  // nvidia-smi style
  CHECK_EQ(chipagent::DeviceKey("GPU-1234-ABCD"), std::string("gpu-1234-abcd"));
  CHECK_EQ(chipagent::DeviceKey("tpu0"), std::string("tpu0"));

  chipagent::Device pci;
  pci.vendor = "habana";
  pci.kind = "accelerator";
  pci.name = "Intel Gaudi (Habana) accelerator";
  pci.id = "0000:19:00.0";
  pci.source = "sysfs";
  chipagent::Device other = pci;
  other.id = "0000:1a:00.0";
  chipagent::Device noid;
  noid.vendor = "x";
  noid.name = "no id";
  chipagent::Device plugin;
  plugin.vendor = "habana";
  plugin.kind = "accelerator";
  plugin.name = "Intel Gaudi HL-225";
  plugin.id = "19:00.0";
  plugin.source = "plugin";
  plugin.memory_total_bytes = 96ULL << 30;
  plugin.utilization_percent = 37;

  std::vector<std::unique_ptr<chipagent::Provider>> providers;
  providers.push_back(std::make_unique<FixedProvider>(std::vector<chipagent::Device>{pci, other, noid}));
  providers.push_back(std::make_unique<FixedProvider>(std::vector<chipagent::Device>{plugin, noid}));
  auto ds = chipagent::Collect(providers);
  CHECK_EQ(ds.size(), size_t(4));  // the plugin's card merged into PCI's; id-less devices never merge
  if (ds.size() == 4) {
    CHECK_EQ(ds[0].name, std::string("Intel Gaudi HL-225"));
    CHECK_EQ(ds[0].id, std::string("0000:19:00.0"));
    CHECK_EQ(ds[0].source, std::string("sysfs+plugin"));
    CHECK_EQ(ds[0].memory_total_bytes, uint64_t(96) << 30);
    CHECK(ds[0].utilization_percent && *ds[0].utilization_percent == 37);
    CHECK_EQ(ds[1].name, std::string("Intel Gaudi (Habana) accelerator"));
  }
}

int main() {
#ifndef _WIN32
  std::signal(SIGPIPE, SIG_IGN);
#endif
  TestJsonEscape();
  TestSimulated();
  TestGoldenTelemetry();
  TestClassifyPci();
#ifndef _WIN32
  TestPci();
  TestDeviceTree();
#endif
#if !defined(_WIN32) && !defined(__APPLE__)
  TestReadHostFromProc();
#endif
  TestParseCpuinfo();
  TestReadHostLive();
  TestJson();
  TestPlugins();
  TestCollectMerges();
  TestNvml();
  TestHttp();
  if (failures) {
    std::cerr << failures << " check(s) failed\n";
    return 1;
  }
  std::cout << "all chipagent tests passed\n";
  return 0;
}
