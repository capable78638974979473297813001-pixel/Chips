cmake_minimum_required(VERSION 3.16)
project(chipagent VERSION 0.2.0 LANGUAGES C CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE Release)
endif()
if(MSVC)
  add_compile_options(/W3 /utf-8)
  add_compile_definitions(_CRT_SECURE_NO_WARNINGS)
else()
  add_compile_options(-Wall -Wextra -Wpedantic)
  if(CMAKE_CXX_COMPILER_ID STREQUAL "GNU")
    add_compile_options(-Wno-psabi)  # 32-bit Arm ABI notes about GCC 7 changes; informational only
  endif()
endif()
if(WIN32)
  # Windows 7 is the oldest version with every API chipagent uses.
  add_compile_definitions(_WIN32_WINNT=0x0601 WIN32_LEAN_AND_MEAN NOMINMAX)
endif()

find_package(Threads REQUIRED)

add_library(chipagent_core STATIC
  src/apple.cpp
  src/dynlib.cpp
  src/host.cpp
  src/http.cpp
  src/nvml.cpp
  src/pci.cpp
  src/simulated.cpp
  src/telemetry.cpp
  src/windows.cpp
)
target_include_directories(chipagent_core PUBLIC src)
target_link_libraries(chipagent_core PUBLIC ${CMAKE_DL_LIBS})
if(WIN32)
  target_link_libraries(chipagent_core PUBLIC ws2_32 dxgi dxguid advapi32)
elseif(APPLE)
  target_link_libraries(chipagent_core PUBLIC "-framework IOKit" "-framework CoreFoundation")
endif()

add_executable(chipagent src/main.cpp)
target_compile_definitions(chipagent PRIVATE CHIPAGENT_VERSION="${PROJECT_VERSION}")
target_link_libraries(chipagent PRIVATE chipagent_core)
install(TARGETS chipagent RUNTIME DESTINATION bin)

include(CTest)
if(BUILD_TESTING)
  # Stands in for libnvidia-ml so the NVML path is tested without a GPU.
  add_library(fakenvml SHARED tests/fake_nvml.c)
  add_executable(chipagent_test tests/agent_test.cpp)
  target_link_libraries(chipagent_test PRIVATE chipagent_core Threads::Threads)
  target_compile_definitions(chipagent_test PRIVATE
    FAKE_NVML_PATH="$<TARGET_FILE:fakenvml>"
    TESTDATA_DIR="${CMAKE_CURRENT_SOURCE_DIR}/tests/testdata")
  add_dependencies(chipagent_test fakenvml)
  add_test(NAME chipagent_test COMMAND chipagent_test)
endif()
