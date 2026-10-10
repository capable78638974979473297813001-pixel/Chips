// Runtime loading of vendor libraries (NVML), so the agent never needs a
// vendor SDK at build time and still starts on machines without the driver.
#pragma once

#include <string>

namespace chipagent {

void* OpenLibrary(const std::string& path);  // nullptr if missing
void* LibrarySymbol(void* lib, const char* name);
void CloseLibrary(void* lib);

}  // namespace chipagent
