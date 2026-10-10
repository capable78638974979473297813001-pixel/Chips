// The host CPU and memory. Any CPU can run small models (llama.cpp builds
// for x86, ARM, POWER, s390x, RISC-V, LoongArch and more), so even a machine
// with no accelerator is reported as capacity.
#include <algorithm>
#include <cctype>
#include <cstdlib>
#include <fstream>
#include <iterator>
#include <map>
#include <sstream>

#include "chipagent.hpp"

#ifdef _WIN32
#include <windows.h>
#else
#include <sys/utsname.h>
#include <unistd.h>
#endif

#ifdef __APPLE__
#include <mach/mach.h>
#include <sys/sysctl.h>
#endif

namespace chipagent {
namespace {

std::string Trim(std::string s) {
  auto notspace = [](unsigned char c) { return !std::isspace(c); };
  s.erase(s.begin(), std::find_if(s.begin(), s.end(), notspace));
  s.erase(std::find_if(s.rbegin(), s.rend(), notspace).base(), s.end());
  return s;
}

std::string Lower(std::string s) {
  for (char& c : s) c = static_cast<char>(std::tolower(static_cast<unsigned char>(c)));
  return s;
}

// Arm CPU names from the implementer and part codes that 64-bit Arm Linux
// reports instead of a model name.
std::string ArmName(const std::string& implementer, const std::string& part) {
  static const std::map<std::pair<unsigned, unsigned>, const char*> kParts = {
      {{0x41, 0xd03}, "Arm Cortex-A53"},     {{0x41, 0xd08}, "Arm Cortex-A72"},
      {{0x41, 0xd0b}, "Arm Cortex-A76"},     {{0x41, 0xd0c}, "Arm Neoverse N1"},
      {{0x41, 0xd40}, "Arm Neoverse V1"},    {{0x41, 0xd49}, "Arm Neoverse N2"},
      {{0x41, 0xd4f}, "Arm Neoverse V2"},    {{0x46, 0x001}, "Fujitsu A64FX"},
      {{0x48, 0xd01}, "HiSilicon Kunpeng 920"},
  };
  static const std::map<unsigned, const char*> kImplementers = {
      {0x41, "Arm"},   {0x42, "Broadcom"}, {0x43, "Cavium"},  {0x46, "Fujitsu"}, {0x48, "HiSilicon"},
      {0x4e, "NVIDIA"}, {0x50, "APM"},     {0x51, "Qualcomm"}, {0x61, "Apple"},  {0xc0, "Ampere"},
  };
  if (implementer.empty()) return "";
  unsigned impl = static_cast<unsigned>(std::strtoul(implementer.c_str(), nullptr, 0));
  unsigned p = static_cast<unsigned>(std::strtoul(part.c_str(), nullptr, 0));
  if (auto it = kParts.find({impl, p}); it != kParts.end()) return it->second;
  auto vit = kImplementers.find(impl);
  std::string who = vit != kImplementers.end() ? vit->second : "Arm implementer " + implementer;
  return who + " CPU part " + part;
}

// Instruction-set extensions that change CPU inference speed, by the names
// Linux uses in /proc/cpuinfo.
const char* const kAiFeatures[] = {
    "sse2",       "avx",       "avx2",        "fma",       "f16c",       "avx_vnni",
    "avx512f",    "avx512bw",  "avx512_vnni", "avx512_bf16", "avx512_fp16", "amx_tile",
    "amx_bf16",   "amx_int8",  "asimd",       "asimddp",   "asimdhp",    "i8mm",
    "bf16",       "sve",       "sve2",        "sme",       "neon",       "vfpv4",
    "altivec",    "vsx",       "mma",         "vx",        "vxe",        "lsx",
    "lasx",
};

bool IsAiFeature(const std::string& f) {
  return std::find(std::begin(kAiFeatures), std::end(kAiFeatures), f) != std::end(kAiFeatures);
}

}  // namespace

void ParseCpuinfo(const std::string& text, std::string* name, std::vector<std::string>* features) {
  // Keep the first value of each key: later per-core blocks repeat it, and
  // some layouts (s390x, older 32-bit Arm) put shared keys outside the blocks.
  std::map<std::string, std::string> kv;
  std::istringstream in(text);
  for (std::string line; std::getline(in, line);) {
    size_t colon = line.find(':');
    if (colon == std::string::npos) continue;
    kv.emplace(Lower(Trim(line.substr(0, colon))), Trim(line.substr(colon + 1)));
  }
  auto get = [&](const char* k) {
    auto it = kv.find(k);
    return it == kv.end() ? std::string() : it->second;
  };

  // Each architecture names the CPU under a different key:
  //   x86, LoongArch, 32-bit Arm: "model name"   MIPS: "cpu model"
  //   RISC-V: "uarch" (else "isa")                POWER, SPARC: "cpu"
  //   64-bit Arm: implementer + part codes        s390x: "vendor_id" + machine type
  *name = get("model name");
  if (name->empty()) name->assign(get("cpu model"));
  if (name->empty()) name->assign(get("uarch"));
  if (name->empty()) name->assign(get("cpu"));
  if (name->empty()) name->assign(ArmName(get("cpu implementer"), get("cpu part")));
  if (name->empty() && !get("isa").empty()) name->assign("RISC-V " + get("isa"));
  if (name->empty() && get("vendor_id") == "IBM/S390") {
    std::string machine;
    size_t m = text.find("machine = ");
    if (m != std::string::npos) machine = text.substr(m + 10, text.find_first_of(" ,\n", m + 10) - m - 10);
    name->assign("IBM Z" + (machine.empty() ? std::string() : " (machine " + machine + ")"));
  }
  // Older 32-bit Arm kernels: "Processor : ARMv7 Processor rev 4 (v7l)"
  // (elsewhere "processor" is just the core number).
  std::string proc = get("processor");
  if (name->empty() && !proc.empty() && !std::isdigit(static_cast<unsigned char>(proc[0]))) name->assign(proc);

  features->clear();
  std::string flags = get("flags");
  if (flags.empty()) flags = get("features");
  std::istringstream fs(flags);
  for (std::string f; fs >> f;) {
    if (IsAiFeature(f) && std::find(features->begin(), features->end(), f) == features->end()) {
      features->push_back(f);
    }
  }
  if (Lower(get("cpu")).find("altivec") != std::string::npos) features->push_back("altivec");
  // RISC-V vector extension: the "v" in an ISA string like rv64imafdcv_zicsr.
  std::string isa = Lower(get("isa"));
  if (isa.rfind("rv", 0) == 0) {
    std::string base = isa.substr(4, isa.find('_') == std::string::npos ? std::string::npos : isa.find('_') - 4);
    if (base.find('v') != std::string::npos) features->push_back("rvv");
  }
}

Host ReadHost(const std::string& root) {
  Host h;
#if defined(_WIN32)
  h.os = "windows";
  char name[256] = {};
  DWORD len = sizeof name;
  if (GetComputerNameExA(ComputerNameDnsHostname, name, &len)) h.hostname = name;
  SYSTEM_INFO si;
  GetNativeSystemInfo(&si);
  switch (si.wProcessorArchitecture) {
    case PROCESSOR_ARCHITECTURE_AMD64: h.arch = "x86_64"; break;
    case PROCESSOR_ARCHITECTURE_INTEL: h.arch = "x86"; break;
    case PROCESSOR_ARCHITECTURE_ARM: h.arch = "arm"; break;
    case 12: h.arch = "aarch64"; break;  // PROCESSOR_ARCHITECTURE_ARM64
    default: h.arch = "unknown";
  }
  char cpu[256] = {};
  DWORD cpu_len = sizeof cpu;
  if (RegGetValueA(HKEY_LOCAL_MACHINE, "HARDWARE\\DESCRIPTION\\System\\CentralProcessor\\0",
                   "ProcessorNameString", RRF_RT_REG_SZ, nullptr, cpu, &cpu_len) == ERROR_SUCCESS) {
    h.cpu = Trim(cpu);
  }
  h.logical_cpus = GetActiveProcessorCount(ALL_PROCESSOR_GROUPS);
  MEMORYSTATUSEX ms;
  ms.dwLength = sizeof ms;
  if (GlobalMemoryStatusEx(&ms)) {
    h.memory_total_bytes = ms.ullTotalPhys;
    h.memory_used_bytes = ms.ullTotalPhys - ms.ullAvailPhys;
  }
  (void)root;
#else
  char name[256] = {};
  if (gethostname(name, sizeof name - 1) == 0) h.hostname = name;
  utsname u{};
  if (uname(&u) == 0) {
    h.os = Lower(u.sysname);
    h.arch = u.machine;
  }
  long cpus = sysconf(_SC_NPROCESSORS_ONLN);
  if (cpus > 0) h.logical_cpus = static_cast<unsigned>(cpus);
#if defined(_SC_PHYS_PAGES)
  long pages = sysconf(_SC_PHYS_PAGES), page = sysconf(_SC_PAGESIZE);
  if (pages > 0 && page > 0) h.memory_total_bytes = uint64_t(pages) * uint64_t(page);
#endif

#if defined(__APPLE__)
  char brand[256] = {};
  size_t blen = sizeof brand;
  if (sysctlbyname("machdep.cpu.brand_string", brand, &blen, nullptr, 0) == 0) h.cpu = brand;
  uint64_t mem = 0;
  size_t mlen = sizeof mem;
  if (sysctlbyname("hw.memsize", &mem, &mlen, nullptr, 0) == 0) h.memory_total_bytes = mem;
  vm_statistics64_data_t vm{};
  mach_msg_type_number_t count = HOST_VM_INFO64_COUNT;
  if (host_statistics64(mach_host_self(), HOST_VM_INFO64, reinterpret_cast<host_info64_t>(&vm), &count) ==
      KERN_SUCCESS) {
    h.memory_used_bytes = (uint64_t(vm.active_count) + vm.wire_count + vm.compressor_page_count) * vm_page_size;
  }
  if (h.arch == "arm64") h.cpu_features = {"asimd", "neon"};  // every Apple silicon core has NEON
  (void)root;
#else
  // Linux and other /proc systems.
  std::ifstream cpuinfo(root + "/proc/cpuinfo");
  if (cpuinfo) {
    std::stringstream ss;
    ss << cpuinfo.rdbuf();
    ParseCpuinfo(ss.str(), &h.cpu, &h.cpu_features);
  }
  std::ifstream meminfo(root + "/proc/meminfo");
  std::string key;
  uint64_t kb = 0, total = 0, available = 0;
  while (meminfo >> key >> kb) {
    if (key == "MemTotal:") total = kb * 1024;
    if (key == "MemAvailable:") available = kb * 1024;
    meminfo.ignore(1 << 10, '\n');
  }
  if (total) h.memory_total_bytes = total;
  if (total && available) h.memory_used_bytes = total - available;
#endif
#endif
  return h;
}

}  // namespace chipagent
