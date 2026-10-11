// A small JSON reader for plugin output. chipagent has no dependencies, and
// plugin documents are small, so a recursive-descent parser is plenty.
#pragma once

#include <map>
#include <optional>
#include <string>
#include <vector>

namespace chipagent {

struct Json {
  enum Type { Null, Bool, Number, String, Array, Object } type = Null;
  bool boolean = false;
  double number = 0;
  std::string string;
  std::vector<Json> array;
  std::map<std::string, Json> object;

  const Json* Get(const std::string& key) const;
  std::optional<double> Num(const std::string& key) const;   // number field, if present
  std::string Str(const std::string& key) const;             // string field, or ""
};

// Parses text as JSON; nullopt (and err) on malformed input.
std::optional<Json> ParseJson(const std::string& text, std::string* err);

}  // namespace chipagent
