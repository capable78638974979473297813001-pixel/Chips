// Plugin providers: a command (a vendor tool wrapped in a script) that
// prints this machine's devices as JSON. This is how chipagent reads chips
// it has no built-in support for: Intel Gaudi via hl-smi, AWS Inferentia
// and Trainium via neuron-ls, or anything else a script can query.
//
// The command prints either {"devices": [...]} or a bare array, each device
// an object with chipagent's field names (vendor, kind, name, id,
// memory_total_bytes, memory_used_bytes, unified_memory,
// utilization_percent, temperature_c, power_watts). Unknown fields are
// ignored and missing ones are reported as unknown.
#include <chrono>
#include <cstdio>
#include <iostream>

#include "chipagent.hpp"
#include "json.hpp"

#ifndef _WIN32
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <sys/wait.h>
#include <unistd.h>
#endif

namespace chipagent {

bool RunCommand(const std::string& cmd, int timeout_ms, std::string* out, std::string* err) {
  out->clear();
#ifdef _WIN32
  // _popen has no timeout; a hanging plugin delays this telemetry request.
  (void)timeout_ms;
  FILE* f = _popen(cmd.c_str(), "r");
  if (!f) {
    *err = "cannot start command";
    return false;
  }
  char buf[4096];
  for (size_t n; (n = fread(buf, 1, sizeof buf, f)) > 0 && out->size() < (8u << 20);) out->append(buf, n);
  int status = _pclose(f);
  if (status != 0) {
    *err = "exited with status " + std::to_string(status);
    return false;
  }
  return true;
#else
  int fds[2];
  if (pipe(fds) != 0) {
    *err = "pipe failed";
    return false;
  }
  pid_t pid = fork();
  if (pid < 0) {
    close(fds[0]);
    close(fds[1]);
    *err = "fork failed";
    return false;
  }
  if (pid == 0) {
    setpgid(0, 0);  // own process group, so a timeout kills the whole pipeline
    dup2(fds[1], STDOUT_FILENO);
    close(fds[0]);
    close(fds[1]);
    execl("/bin/sh", "sh", "-c", cmd.c_str(), static_cast<char*>(nullptr));
    _exit(127);
  }
  close(fds[1]);
  auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(timeout_ms);
  bool timed_out = false;
  char buf[4096];
  for (;;) {
    auto left = std::chrono::duration_cast<std::chrono::milliseconds>(deadline - std::chrono::steady_clock::now()).count();
    if (left <= 0) {
      timed_out = true;
      break;
    }
    pollfd p{fds[0], POLLIN, 0};
    int r = poll(&p, 1, static_cast<int>(left));
    if (r == 0) {
      timed_out = true;
      break;
    }
    if (r < 0) continue;  // EINTR
    ssize_t n = read(fds[0], buf, sizeof buf);
    if (n <= 0) break;  // EOF: the command closed its output
    if (out->size() < (8u << 20)) out->append(buf, static_cast<size_t>(n));
  }
  close(fds[0]);
  if (timed_out) kill(-pid, SIGKILL);
  int status = 0;
  waitpid(pid, &status, 0);
  if (timed_out) {
    *err = "timed out after " + std::to_string(timeout_ms) + " ms";
    return false;
  }
  if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) {
    *err = WIFEXITED(status) ? "exited with status " + std::to_string(WEXITSTATUS(status)) : "killed by a signal";
    return false;
  }
  return true;
#endif
}

namespace {

class PluginProvider : public Provider {
 public:
  PluginProvider(std::string cmd, int timeout_ms) : cmd_(std::move(cmd)), timeout_ms_(timeout_ms) {}

  std::string Name() const override { return "plugin"; }

  std::vector<Device> Devices() override {
    std::string out, err;
    if (!RunCommand(cmd_, timeout_ms_, &out, &err)) {
      std::cerr << "chipagent: plugin \"" << cmd_ << "\": " << err << "\n";
      return {};
    }
    auto doc = ParseJson(out, &err);
    if (!doc) {
      std::cerr << "chipagent: plugin \"" << cmd_ << "\" printed invalid JSON: " << err << "\n";
      return {};
    }
    const Json* list = doc->type == Json::Array ? &*doc : doc->Get("devices");
    if (!list || list->type != Json::Array) {
      std::cerr << "chipagent: plugin \"" << cmd_ << "\" printed no devices array\n";
      return {};
    }
    std::vector<Device> devices;
    for (const auto& j : list->array) {
      if (j.type != Json::Object) continue;
      Device d;
      d.vendor = j.Str("vendor");
      d.name = j.Str("name");
      if (d.vendor.empty() || d.name.empty()) continue;  // too little to be useful
      d.kind = j.Str("kind");
      if (d.kind != "gpu" && d.kind != "npu" && d.kind != "accelerator" && d.kind != "display") d.kind = "accelerator";
      d.id = j.Str("id");
      d.source = "plugin";
      if (auto v = j.Num("memory_total_bytes"); v && *v >= 0) d.memory_total_bytes = static_cast<uint64_t>(*v);
      if (auto v = j.Num("memory_used_bytes"); v && *v >= 0) d.memory_used_bytes = static_cast<uint64_t>(*v);
      const Json* um = j.Get("unified_memory");
      d.unified_memory = um && um->type == Json::Bool && um->boolean;
      d.utilization_percent = j.Num("utilization_percent");
      d.temperature_c = j.Num("temperature_c");
      d.power_watts = j.Num("power_watts");
      devices.push_back(std::move(d));
    }
    return devices;
  }

 private:
  std::string cmd_;
  int timeout_ms_;
};

}  // namespace

std::unique_ptr<Provider> OpenPlugin(const std::string& command, int timeout_ms) {
  return std::make_unique<PluginProvider>(command, timeout_ms);
}

}  // namespace chipagent
