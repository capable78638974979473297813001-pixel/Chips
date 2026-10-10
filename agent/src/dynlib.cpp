#include "dynlib.hpp"

#ifdef _WIN32
#include <windows.h>

namespace chipagent {

void* OpenLibrary(const std::string& path) {
  std::string p = path;
  for (char& c : p) {
    if (c == '/') c = '\\';
  }
  return reinterpret_cast<void*>(::LoadLibraryA(p.c_str()));
}

void* LibrarySymbol(void* lib, const char* name) {
  return reinterpret_cast<void*>(::GetProcAddress(static_cast<HMODULE>(lib), name));
}

void CloseLibrary(void* lib) { ::FreeLibrary(static_cast<HMODULE>(lib)); }

}  // namespace chipagent

#else
#include <dlfcn.h>

namespace chipagent {

void* OpenLibrary(const std::string& path) { return dlopen(path.c_str(), RTLD_NOW | RTLD_LOCAL); }

void* LibrarySymbol(void* lib, const char* name) { return dlsym(lib, name); }

void CloseLibrary(void* lib) { dlclose(lib); }

}  // namespace chipagent
#endif
