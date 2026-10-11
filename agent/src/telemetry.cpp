#include <cctype>
#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <condition_variable>
#include <map>
#include <mutex>
#include <optional>
#include <thread>

#include "chipagent.hpp"

namespace chipagent {

namespace {

bool IsHex(std::string_view s) {
  if (s.empty()) return false;
  for (char c : s) {
    if (!std::isxdigit(static_cast<unsigned char>(c))) return false;
  }
  return true;
}

}  // namespace

std::string DeviceKey(std::string_view id) {
  std::string key;
  for (char c : id) key += static_cast<char>(std::tolower(static_cast<unsigned char>(c)));
  // A PCI address, with or without its domain: [dddd:]bb:dd.f
  std::string_view k = key;
  size_t dot = k.rfind('.');
  size_t colon = k.rfind(':');
  if (dot == std::string_view::npos || colon == std::string_view::npos || colon > dot) return key;
  std::string_view head = k.substr(0, colon);
  size_t first = head.find(':');
  std::string_view domain = first == std::string_view::npos ? std::string_view{} : head.substr(0, first);
  std::string_view bus = first == std::string_view::npos ? head : head.substr(first + 1);
  std::string_view slot = k.substr(colon + 1, dot - colon - 1);
  std::string_view fn = k.substr(dot + 1);
  if (!IsHex(bus) || !IsHex(slot) || !IsHex(fn) || (!domain.empty() && !IsHex(domain)) || bus.size() > 2 ||
      slot.size() > 2 || fn.size() > 1 || domain.size() > 8) {
    return key;
  }
  auto hex = [](std::string_view v) { return v.empty() ? 0UL : std::strtoul(std::string(v).c_str(), nullptr, 16); };
  char buf[32];
  std::snprintf(buf, sizeof buf, "%04lx:%02lx:%02lx.%lx", hex(domain), hex(bus), hex(slot), hex(fn));
  return buf;
}

// Several providers can report one device: PCI sysfs lists every card, and
// a plugin around the vendor's tool reports the same card with readings.
// Devices with the same id (PCI addresses compared with or without their
// domain) are merged: the later provider's values win, and values it lacks
// are kept from the earlier one.
std::vector<Device> Collect(const std::vector<std::unique_ptr<Provider>>& providers) {
  std::vector<Device> out;
  std::map<std::string, size_t> byKey;
  for (const auto& p : providers) {
    for (auto& d : p->Devices()) {
      std::string key = d.id.empty() ? std::string() : DeviceKey(d.id);
      auto it = key.empty() ? byKey.end() : byKey.find(key);
      if (it == byKey.end()) {
        if (!key.empty()) byKey[key] = out.size();
        out.push_back(std::move(d));
        continue;
      }
      Device& prev = out[it->second];
      if (!d.vendor.empty()) prev.vendor = d.vendor;
      if (!d.kind.empty()) prev.kind = d.kind;
      if (!d.name.empty()) prev.name = d.name;
      prev.source = prev.source + "+" + d.source;
      if (d.memory_total_bytes) prev.memory_total_bytes = d.memory_total_bytes;
      if (d.memory_used_bytes) prev.memory_used_bytes = d.memory_used_bytes;
      prev.unified_memory = prev.unified_memory || d.unified_memory;
      if (d.utilization_percent) prev.utilization_percent = d.utilization_percent;
      if (d.temperature_c) prev.temperature_c = d.temperature_c;
      if (d.power_watts) prev.power_watts = d.power_watts;
    }
  }
  return out;
}

std::string JsonEscape(std::string_view s) {
  std::string out;
  out.reserve(s.size() + 2);
  for (unsigned char c : s) {
    switch (c) {
      case '"': out += "\\\""; break;
      case '\\': out += "\\\\"; break;
      case '\n': out += "\\n"; break;
      case '\r': out += "\\r"; break;
      case '\t': out += "\\t"; break;
      default:
        if (c < 0x20) {
          char buf[8];
          std::snprintf(buf, sizeof buf, "\\u%04x", c);
          out += buf;
        } else {
          out += static_cast<char>(c);
        }
    }
  }
  return out;
}

namespace {

std::string Str(std::string_view s) { return "\"" + JsonEscape(s) + "\""; }

std::string Num(const std::optional<double>& v) {
  if (!v || !std::isfinite(*v)) return "null";
  char buf[32];
  std::snprintf(buf, sizeof buf, "%.10g", *v);
  return buf;
}

}  // namespace

std::string RenderTelemetry(const std::vector<Device>& devices, const Host& host,
                            const std::string& version, const std::string& collect_error) {
  std::string out = "{\"schema\":1,\"agent\":\"chipagent\",\"version\":" + Str(version) +
                    ",\"hostname\":" + Str(host.hostname);
  out += ",\"host\":{\"os\":" + Str(host.os) + ",\"arch\":" + Str(host.arch) + ",\"cpu\":" + Str(host.cpu);
  out += ",\"logical_cpus\":" + std::to_string(host.logical_cpus);
  out += ",\"memory_total_bytes\":" + std::to_string(host.memory_total_bytes);
  out += ",\"memory_used_bytes\":" + std::to_string(host.memory_used_bytes);
  out += ",\"cpu_features\":[";
  for (size_t i = 0; i < host.cpu_features.size(); ++i) out += (i ? "," : "") + Str(host.cpu_features[i]);
  out += "]},\"devices\":[";
  for (size_t i = 0; i < devices.size(); ++i) {
    const Device& d = devices[i];
    if (i > 0) out += ',';
    out += "{\"index\":" + std::to_string(i);
    out += ",\"vendor\":" + Str(d.vendor);
    out += ",\"kind\":" + Str(d.kind);
    out += ",\"name\":" + Str(d.name);
    out += ",\"id\":" + Str(d.id);
    out += ",\"source\":" + Str(d.source);
    out += ",\"memory_total_bytes\":" + std::to_string(d.memory_total_bytes);
    out += ",\"memory_used_bytes\":" + std::to_string(d.memory_used_bytes);
    out += std::string(",\"unified_memory\":") + (d.unified_memory ? "true" : "false");
    out += ",\"utilization_percent\":" + Num(d.utilization_percent);
    out += ",\"temperature_c\":" + Num(d.temperature_c);
    out += ",\"power_watts\":" + Num(d.power_watts);
    if (!d.error.empty()) out += ",\"error\":" + Str(d.error);
    out += '}';
  }
  out += ']';
  if (!collect_error.empty()) out += ",\"stale\":true,\"collect_error\":" + Str(collect_error);
  out += "}\n";
  return out;
}

struct Collector::State {
  std::mutex mu;
  std::condition_variable done;
  bool busy = false;
  uint64_t generation = 0;  // completed collections
  std::optional<Snapshot> last;
  std::chrono::steady_clock::time_point last_at;
};

Collector::Collector(std::function<Snapshot()> collect, std::chrono::milliseconds deadline)
    : state_(std::make_shared<State>()), collect_(std::move(collect)), deadline_(deadline) {}

std::string Collector::Telemetry(const std::string& version) {
  std::unique_lock<std::mutex> lock(state_->mu);
  const uint64_t start = state_->generation;
  if (!state_->busy) {
    state_->busy = true;
    std::thread([st = state_, collect = collect_] {
      std::optional<Snapshot> s;
      try {
        s = collect();
      } catch (...) {  // a provider bug: keep the last good readings
      }
      std::lock_guard<std::mutex> g(st->mu);
      if (s) {
        st->last = std::move(s);
        st->last_at = std::chrono::steady_clock::now();
      }
      st->busy = false;
      ++st->generation;
      st->done.notify_all();
    }).detach();
  }
  bool finished = state_->done.wait_for(lock, deadline_, [&] { return state_->generation != start; });
  // Fresh: a collection succeeded during this call (it may have been one a
  // previous call started and gave up waiting for).
  if (finished && state_->last && std::chrono::steady_clock::now() - state_->last_at < deadline_) {
    return RenderTelemetry(state_->last->devices, state_->last->host, version);
  }
  std::string why = finished ? "collection failed" : "collection did not finish within " +
                                                         std::to_string(deadline_.count()) +
                                                         " ms (a driver or sysfs read is hanging)";
  if (!state_->last) return RenderTelemetry({}, Host{}, version, why + "; no readings yet");
  auto age = std::chrono::duration_cast<std::chrono::seconds>(std::chrono::steady_clock::now() - state_->last_at);
  return RenderTelemetry(state_->last->devices, state_->last->host, version,
                         why + "; readings are " + std::to_string(age.count()) + " s old");
}

}  // namespace chipagent
