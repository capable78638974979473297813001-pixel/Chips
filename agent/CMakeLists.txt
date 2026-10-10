cmake_minimum_required(VERSION 3.16)
project(chipagent VERSION 0.1.0 LANGUAGES C CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE Release)
endif()
add_compile_options(-Wall -Wextra -Wpedantic)

find_package(Threads REQUIRED)

add_library(chipagent_core STATIC
  src/apple.cpp
  src/http.cpp
  src/nvml.cpp
  src/simulated.cpp
  src/sysfs.cpp
  src/telemetry.cpp
)
target_include_directories(chipagent_core PUBLIC src)
target_link_libraries(chipagent_core PUBLIC ${CMAKE_DL_LIBS})

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
