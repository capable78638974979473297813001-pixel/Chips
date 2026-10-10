// Unit tests for chipagent. No framework: each CHECK failure prints and the
// process exits non-zero at the end.
#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <unistd.h>

#include <cmath>
#include <csignal>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <sstream>
#include <thread>

#include "chipagent.hpp"

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

static void Write(const fs::path& p, const std::string& content) {
  fs::create_directories(p.parent_path());
  std::ofstream(p) << content;
}

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

  for (const char* bad : {"nvidia", "nvidia:H100", ":H100:80", "nvidia:H100:lots", "nvidia:H100:-1",
                          "nvidia:H100:80:1:2"}) {
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
  std::string got = chipagent::RenderTelemetry(devices, "gpu-node-1", "0.1.0");
  std::string want = ReadFile(fs::path(TESTDATA_DIR) / "telemetry.json");
  CHECK_EQ(got, want);
}

static void TestSysfs() {
  fs::path root = fs::temp_directory_path() / ("chipagent-sysfs-" + std::to_string(getpid()));
  fs::remove_all(root);
  fs::path pci = root / "sys/devices/pci0000:00";
  fs::path drm = root / "sys/class/drm";
  auto card = [&](const std::string& name, const std::string& bus) {
    fs::create_directories(pci / bus);
    fs::create_directories(drm / name);
    fs::create_symlink(fs::path("../../../devices/pci0000:00") / bus, drm / name / "device");
    return pci / bus;
  };

  fs::path amd = card("card1", "0000:03:00.0");
  Write(amd / "vendor", "0x1002\n");
  Write(amd / "device", "0x74a1\n");
  Write(amd / "mem_info_vram_total", "206141652992\n");
  Write(amd / "mem_info_vram_used", "1073741824\n");
  Write(amd / "gpu_busy_percent", "42\n");
  Write(amd / "hwmon/hwmon3/temp1_input", "51000\n");
  Write(amd / "hwmon/hwmon3/power1_average", "350000000\n");

  fs::path intel = card("card0", "0000:00:02.0");
  Write(intel / "vendor", "0x8086\n");
  Write(intel / "device", "0x56a0\n");

  fs::path nv = card("card2", "0000:41:00.0");
  Write(nv / "vendor", "0x10de\n");
  Write(nv / "device", "0x2330\n");

  fs::path bmc = card("card3", "0000:02:00.0");  // ASPEED display adapter
  Write(bmc / "vendor", "0x1a03\n");

  fs::create_directories(drm / "card1-DP-1");  // connectors are not devices
  fs::create_directories(drm / "renderD128");

  auto ds = chipagent::OpenSysfs(root.string(), true)->Devices();
  CHECK_EQ(ds.size(), size_t(2));
  if (ds.size() == 2) {
    CHECK_EQ(ds[0].vendor, std::string("intel"));
    CHECK_EQ(ds[0].name, std::string("Intel GPU [8086:56a0]"));
    CHECK_EQ(ds[0].memory_total_bytes, uint64_t(0));
    CHECK_EQ(ds[1].vendor, std::string("amd"));
    CHECK_EQ(ds[1].name, std::string("AMD Instinct MI300X"));
    CHECK_EQ(ds[1].id, std::string("0000:03:00.0"));
    CHECK_EQ(ds[1].memory_total_bytes, uint64_t(206141652992));
    CHECK_EQ(ds[1].memory_used_bytes, uint64_t(1073741824));
    CHECK(ds[1].utilization_percent && *ds[1].utilization_percent == 42);
    CHECK(ds[1].temperature_c && *ds[1].temperature_c == 51);
    CHECK(ds[1].power_watts && *ds[1].power_watts == 350);
  }

  // product_name wins over the built-in ID table.
  Write(amd / "product_name", "AMD Instinct MI300X OAM\n");
  ds = chipagent::OpenSysfs(root.string(), false)->Devices();
  CHECK_EQ(ds.size(), size_t(3));
  if (ds.size() == 3) {
    CHECK_EQ(ds[1].name, std::string("AMD Instinct MI300X OAM"));
    CHECK_EQ(ds[2].vendor, std::string("nvidia"));
    CHECK_EQ(ds[2].source, std::string("sysfs"));
  }

  CHECK(chipagent::OpenSysfs((root / "missing").string(), true)->Devices().empty());
  fs::remove_all(root);
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

static std::string HttpGet(int port, const std::string& request) {
  int fd = socket(AF_INET, SOCK_STREAM, 0);
  sockaddr_in addr{};
  addr.sin_family = AF_INET;
  addr.sin_port = htons(static_cast<uint16_t>(port));
  addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  if (connect(fd, reinterpret_cast<sockaddr*>(&addr), sizeof addr) != 0) {
    close(fd);
    return "";
  }
  send(fd, request.data(), request.size(), 0);
  std::string out;
  char buf[4096];
  for (ssize_t n; (n = recv(fd, buf, sizeof buf, 0)) > 0;) out.append(buf, static_cast<size_t>(n));
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

int main() {
  std::signal(SIGPIPE, SIG_IGN);
  TestJsonEscape();
  TestSimulated();
  TestGoldenTelemetry();
  TestSysfs();
  TestNvml();
  TestHttp();
  if (failures) {
    std::cerr << failures << " check(s) failed\n";
    return 1;
  }
  std::cout << "all chipagent tests passed\n";
  return 0;
}
