#include <cstdlib>
#include <sstream>

#include "chipagent.hpp"

namespace chipagent {
namespace {

class SimulatedProvider : public Provider {
 public:
  explicit SimulatedProvider(std::vector<Device> devices) : devices_(std::move(devices)) {}
  std::string Name() const override { return "simulated"; }
  std::vector<Device> Devices() override { return devices_; }

 private:
  std::vector<Device> devices_;
};

bool ParseNumber(const std::string& s, double* out) {
  char* end = nullptr;
  *out = std::strtod(s.c_str(), &end);
  return !s.empty() && *end == '\0' && *out >= 0;
}

}  // namespace

std::unique_ptr<Provider> OpenSimulated(const std::vector<std::string>& specs, std::string* err) {
  std::vector<Device> devices;
  for (const auto& spec : specs) {
    std::vector<std::string> parts;
    std::stringstream ss(spec);
    for (std::string part; std::getline(ss, part, ':');) parts.push_back(part);
    double gib = 0, util = 0;
    if (parts.size() < 3 || parts.size() > 4 || parts[0].empty() || parts[1].empty() ||
        !ParseNumber(parts[2], &gib) || (parts.size() == 4 && !ParseNumber(parts[3], &util))) {
      *err = "invalid --simulate spec \"" + spec + "\"; want vendor:name:memory_gib[:utilization_percent]";
      return nullptr;
    }
    Device d;
    d.vendor = parts[0];
    d.name = parts[1];
    d.id = "sim-" + std::to_string(devices.size());
    d.source = "simulated";
    d.memory_total_bytes = static_cast<uint64_t>(gib * (1ull << 30));
    d.unified_memory = d.vendor == "apple";
    if (parts.size() == 4) d.utilization_percent = util;
    devices.push_back(std::move(d));
  }
  return std::make_unique<SimulatedProvider>(std::move(devices));
}

}  // namespace chipagent
