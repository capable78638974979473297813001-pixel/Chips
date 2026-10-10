// Command chipagent reports the accelerators in this machine (NVIDIA, AMD,
// Intel, Apple) over HTTP so SiliconRouter can tier and route across a mixed
// fleet without hand-written hardware labels.
#include <unistd.h>

#include <csignal>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <iostream>

#include "chipagent.hpp"

#ifndef CHIPAGENT_VERSION
#define CHIPAGENT_VERSION "dev"
#endif

namespace {

std::atomic<bool> g_stop{false};

void OnSignal(int) { g_stop = true; }

const char* kUsage = R"(usage: chipagent [flags]

Serves GET /v1/telemetry (JSON) and GET /healthz.

  --listen ADDR         address to serve on (default ":9400")
  --once                print the telemetry JSON once and exit
  --simulate SPEC       report a fake device instead of real hardware; repeatable.
                        SPEC is vendor:name:memory_gib[:utilization_percent],
                        e.g. "nvidia:NVIDIA H100 80GB HBM3:80"
  --nvml-library PATH   NVML library to load (default libnvidia-ml.so.1)
  --sysfs-root PATH     root containing sys/class/drm (default "/")
  --version             print the version and exit
)";

std::string Hostname() {
  char buf[256] = {};
  if (gethostname(buf, sizeof buf - 1) != 0) return "";
  return buf;
}

}  // namespace

int main(int argc, char** argv) {
  std::string listen = ":9400", nvml_lib, sysfs_root = "/";
  std::vector<std::string> simulate;
  bool once = false;
  for (int i = 1; i < argc; ++i) {
    std::string arg = argv[i];
    auto value = [&](const char* flag) -> std::string {
      if (i + 1 >= argc) {
        std::cerr << "chipagent: " << flag << " needs a value\n";
        std::exit(2);
      }
      return argv[++i];
    };
    if (arg == "--listen") listen = value("--listen");
    else if (arg == "--once") once = true;
    else if (arg == "--simulate") simulate.push_back(value("--simulate"));
    else if (arg == "--nvml-library") nvml_lib = value("--nvml-library");
    else if (arg == "--sysfs-root") sysfs_root = value("--sysfs-root");
    else if (arg == "--version") {
      std::cout << CHIPAGENT_VERSION << "\n";
      return 0;
    } else if (arg == "-h" || arg == "--help") {
      std::cout << kUsage;
      return 0;
    } else {
      std::cerr << "chipagent: unknown flag " << arg << "\n" << kUsage;
      return 2;
    }
  }

  std::vector<std::unique_ptr<chipagent::Provider>> providers;
  if (!simulate.empty()) {
    std::string err;
    auto sim = chipagent::OpenSimulated(simulate, &err);
    if (!sim) {
      std::cerr << "chipagent: " << err << "\n";
      return 2;
    }
    providers.push_back(std::move(sim));
  } else {
    auto nvml = chipagent::OpenNvml(nvml_lib);
    bool have_nvml = nvml != nullptr;
    if (nvml) providers.push_back(std::move(nvml));
    providers.push_back(chipagent::OpenSysfs(sysfs_root, have_nvml));
    if (auto apple = chipagent::OpenApple()) providers.push_back(std::move(apple));
  }

  const std::string host = Hostname();
  auto telemetry = [&] {
    return chipagent::RenderTelemetry(chipagent::Collect(providers), host, CHIPAGENT_VERSION);
  };
  if (once) {
    std::cout << telemetry();
    return 0;
  }

  for (const auto& d : chipagent::Collect(providers)) {
    std::cerr << "chipagent: found " << d.vendor << " \"" << d.name << "\" via " << d.source;
    if (d.memory_total_bytes) std::cerr << ", " << (d.memory_total_bytes >> 20) << " MiB";
    std::cerr << "\n";
  }

  struct sigaction sa{};
  sa.sa_handler = OnSignal;
  sigaction(SIGINT, &sa, nullptr);
  sigaction(SIGTERM, &sa, nullptr);
  std::signal(SIGPIPE, SIG_IGN);

  chipagent::HttpServer server;
  std::string err;
  if (!server.Listen(listen, &err)) {
    std::cerr << "chipagent: " << err << "\n";
    return 1;
  }
  std::cerr << "chipagent " << CHIPAGENT_VERSION << " listening on port " << server.Port() << "\n";
  server.Serve(
      [&](const std::string& method, const std::string& path) -> chipagent::HttpServer::Response {
        if (method != "GET") return {405, "text/plain", "method not allowed\n"};
        if (path == "/healthz") return {200, "text/plain", "ok\n"};
        if (path == "/v1/telemetry") return {200, "application/json", telemetry()};
        return {404, "text/plain", "not found\n"};
      },
      g_stop);
  return 0;
}
