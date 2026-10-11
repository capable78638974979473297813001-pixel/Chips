#include "json.hpp"

#include <cstdlib>

namespace chipagent {
namespace {

class Parser {
 public:
  explicit Parser(const std::string& s) : s_(s) {}

  std::optional<Json> Document(std::string* err) {
    Json v;
    if (!Value(&v, 0)) {
      *err = err_.empty() ? "malformed JSON" : err_;
      return std::nullopt;
    }
    Space();
    if (i_ != s_.size()) {
      *err = "trailing data after JSON value";
      return std::nullopt;
    }
    return v;
  }

 private:
  void Space() {
    while (i_ < s_.size() && (s_[i_] == ' ' || s_[i_] == '\t' || s_[i_] == '\n' || s_[i_] == '\r')) ++i_;
  }

  bool Literal(const char* word) {
    size_t n = std::char_traits<char>::length(word);
    if (s_.compare(i_, n, word) != 0) return false;
    i_ += n;
    return true;
  }

  bool Value(Json* out, int depth) {
    if (depth > 64) {
      err_ = "JSON nested too deeply";
      return false;
    }
    Space();
    if (i_ >= s_.size()) return false;
    char c = s_[i_];
    if (c == '{') return Object(out, depth);
    if (c == '[') return Array(out, depth);
    if (c == '"') {
      out->type = Json::String;
      return String(&out->string);
    }
    if (Literal("true")) {
      out->type = Json::Bool;
      out->boolean = true;
      return true;
    }
    if (Literal("false")) {
      out->type = Json::Bool;
      return true;
    }
    if (Literal("null")) return true;
    return Number(out);
  }

  bool Number(Json* out) {
    const char* start = s_.c_str() + i_;
    char* end = nullptr;
    double v = std::strtod(start, &end);
    if (end == start) return false;
    i_ += static_cast<size_t>(end - start);
    out->type = Json::Number;
    out->number = v;
    return true;
  }

  static void Utf8(unsigned cp, std::string* out) {
    if (cp < 0x80) {
      *out += static_cast<char>(cp);
    } else if (cp < 0x800) {
      *out += static_cast<char>(0xC0 | (cp >> 6));
      *out += static_cast<char>(0x80 | (cp & 0x3F));
    } else {
      *out += static_cast<char>(0xE0 | (cp >> 12));
      *out += static_cast<char>(0x80 | ((cp >> 6) & 0x3F));
      *out += static_cast<char>(0x80 | (cp & 0x3F));
    }
  }

  bool String(std::string* out) {
    ++i_;  // opening quote
    while (i_ < s_.size()) {
      char c = s_[i_++];
      if (c == '"') return true;
      if (c != '\\') {
        *out += c;
        continue;
      }
      if (i_ >= s_.size()) return false;
      char e = s_[i_++];
      switch (e) {
        case '"': case '\\': case '/': *out += e; break;
        case 'b': *out += '\b'; break;
        case 'f': *out += '\f'; break;
        case 'n': *out += '\n'; break;
        case 'r': *out += '\r'; break;
        case 't': *out += '\t'; break;
        case 'u': {
          if (i_ + 4 > s_.size()) return false;
          unsigned cp = static_cast<unsigned>(std::strtoul(s_.substr(i_, 4).c_str(), nullptr, 16));
          i_ += 4;
          Utf8(cp, out);  // surrogate pairs are not needed for device names
          break;
        }
        default: return false;
      }
    }
    return false;
  }

  bool Array(Json* out, int depth) {
    out->type = Json::Array;
    ++i_;
    Space();
    if (i_ < s_.size() && s_[i_] == ']') {
      ++i_;
      return true;
    }
    for (;;) {
      Json v;
      if (!Value(&v, depth + 1)) return false;
      out->array.push_back(std::move(v));
      Space();
      if (i_ >= s_.size()) return false;
      if (s_[i_] == ']') {
        ++i_;
        return true;
      }
      if (s_[i_++] != ',') return false;
    }
  }

  bool Object(Json* out, int depth) {
    out->type = Json::Object;
    ++i_;
    Space();
    if (i_ < s_.size() && s_[i_] == '}') {
      ++i_;
      return true;
    }
    for (;;) {
      Space();
      std::string key;
      if (i_ >= s_.size() || s_[i_] != '"' || !String(&key)) return false;
      Space();
      if (i_ >= s_.size() || s_[i_++] != ':') return false;
      Json v;
      if (!Value(&v, depth + 1)) return false;
      out->object[key] = std::move(v);
      Space();
      if (i_ >= s_.size()) return false;
      if (s_[i_] == '}') {
        ++i_;
        return true;
      }
      if (s_[i_++] != ',') return false;
    }
  }

  const std::string& s_;
  size_t i_ = 0;
  std::string err_;
};

}  // namespace

const Json* Json::Get(const std::string& key) const {
  auto it = object.find(key);
  return it == object.end() ? nullptr : &it->second;
}

std::optional<double> Json::Num(const std::string& key) const {
  const Json* v = Get(key);
  if (!v || v->type != Number) return std::nullopt;
  return v->number;
}

std::string Json::Str(const std::string& key) const {
  const Json* v = Get(key);
  return v && v->type == String ? v->string : "";
}

std::optional<Json> ParseJson(const std::string& text, std::string* err) { return Parser(text).Document(err); }

}  // namespace chipagent
