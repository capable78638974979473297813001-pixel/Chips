// Command chipagent reports the compute hardware in this machine (CPU, GPUs
// and AI accelerators from any vendor) over HTTP so SiliconRouter can tier
// and route across a mixed fleet without hand-written hardware labels.
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
                        SPEC is vendor:name:memory_gib[:utilization_percent[:kind]],
                        kind one of gpu, npu, accelerator, display (default gpu),
                        e.g. "nvidia:NVIDIA H100 80GB HBM3:80" or "google:TPU v5e:16::accelerator"
  --plugin COMMAND      also report the devices COMMAND prints as JSON; repeatable.
                        Use it for chips without built-in support (see agent/plugins/)
  --plugin-timeout MS   how long a plugin may run per collection (default 5000)
  --nvml-library PATH   NVML library to load (default libnvidia-ml.so.1 / nvml.dll)
  --sysfs-root PATH     Linux: root containing sys/ and proc/ (default "/")
  --version             print the version and exit
)";

}  // namespace

int main(int argc, char** argv) {
  std::string listen = ":9400", nvml_lib, sysfs_root = "/";
  std::vector<std::string> simulate, plugins;
  int plugin_timeout = 5000;
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
    else if (arg == "--plugin") plugins.push_back(value("--plugin"));
    else if (arg == "--plugin-timeout") plugin_timeout = std::atoi(value("--plugin-timeout").c_str());
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
    // NVML gives the richest NVIDIA data; the platform-wide providers below
    // skip NVIDIA devices when it is present so nothing is counted twice.
    auto nvml = chipagent::OpenNvml(nvml_lib);
    bool have_nvml = nvml != nullptr;
    if (nvml) providers.push_back(std::move(nvml));
#if defined(_WIN32)
    if (auto dxgi = chipagent::OpenDxgi(have_nvml)) providers.push_back(std::move(dxgi));
#elif defined(__APPLE__)
    (void)have_nvml;  // no NVML on macOS: NVIDIA's driver there never shipped it
    if (auto apple = chipagent::OpenApple()) providers.push_back(std::move(apple));
#else
    providers.push_back(chipagent::OpenPci(sysfs_root, have_nvml));
    if (!have_nvml) providers.push_back(chipagent::OpenDeviceTree(sysfs_root));
#endif
  }

  for (const auto& cmd : plugins) providers.push_back(chipagent::OpenPlugin(cmd, plugin_timeout > 0 ? plugin_timeout : 5000));

  auto telemetry = [&] {
    return chipagent::RenderTelemetry(chipagent::Collect(providers), chipagent::ReadHost(sysfs_root),
                                      CHIPAGENT_VERSION);
  };
  if (once) {
    std::cout << telemetry();
    return 0;
  }

  chipagent::Host host = chipagent::ReadHost(sysfs_root);
  std::cerr << "chipagent: host " << host.os << "/" << host.arch << " \"" << host.cpu << "\", "
            << host.logical_cpus << " CPUs, " << (host.memory_total_bytes >> 20) << " MiB RAM\n";
  for (const auto& d : chipagent::Collect(providers)) {
    std::cerr << "chipagent: found " << d.kind << " " << d.vendor << " \"" << d.name << "\" via " << d.source;
    if (d.memory_total_bytes) std::cerr << ", " << (d.memory_total_bytes >> 20) << " MiB";
    std::cerr << "\n";
  }

#ifdef _WIN32
  std::signal(SIGINT, OnSignal);
  std::signal(SIGTERM, OnSignal);
#else
  struct sigaction sa{};
  sa.sa_handler = OnSignal;
  sigaction(SIGINT, &sa, nullptr);
  sigaction(SIGTERM, &sa, nullptr);
  std::signal(SIGPIPE, SIG_IGN);
#endif

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
