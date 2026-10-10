#include <cmath>
#include <cstdio>

#include "chipagent.hpp"

namespace chipagent {

std::vector<Device> Collect(const std::vector<std::unique_ptr<Provider>>& providers) {
  std::vector<Device> out;
  for (const auto& p : providers) {
    for (auto& d : p->Devices()) out.push_back(std::move(d));
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

std::string RenderTelemetry(const std::vector<Device>& devices, const std::string& hostname,
                            const std::string& version) {
  std::string out = "{\"schema\":1,\"agent\":\"chipagent\",\"version\":" + Str(version) +
                    ",\"hostname\":" + Str(hostname) + ",\"devices\":[";
  for (size_t i = 0; i < devices.size(); ++i) {
    const Device& d = devices[i];
    if (i > 0) out += ',';
    out += "{\"index\":" + std::to_string(i);
    out += ",\"vendor\":" + Str(d.vendor);
    out += ",\"name\":" + Str(d.name);
    out += ",\"id\":" + Str(d.id);
    out += ",\"source\":" + Str(d.source);
    out += ",\"memory_total_bytes\":" + std::to_string(d.memory_total_bytes);
    out += ",\"memory_used_bytes\":" + std::to_string(d.memory_used_bytes);
    out += std::string(",\"unified_memory\":") + (d.unified_memory ? "true" : "false");
    out += ",\"utilization_percent\":" + Num(d.utilization_percent);
    out += ",\"temperature_c\":" + Num(d.temperature_c);
    out += ",\"power_watts\":" + Num(d.power_watts);
    out += '}';
  }
  out += "]}\n";
  return out;
}

}  // namespace chipagent
