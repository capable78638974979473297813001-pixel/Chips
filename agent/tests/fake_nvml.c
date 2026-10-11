/* A two-GPU stand-in for libnvidia-ml, exporting the NVML entry points
 * chipagent uses with their real C signatures. */
#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#define EXPORT __declspec(dllexport)
#else
#define EXPORT
#endif

typedef struct {
  int index;
} fake_device;

static fake_device devices[2] = {{0}, {1}};
static int initialized;

typedef struct {
  unsigned long long total, free, used;
} nvmlMemory_t;
typedef struct {
  unsigned int version;
  unsigned long long total, reserved, free, used;
} nvmlMemory_v2_t;
typedef struct {
  unsigned int gpu, memory;
} nvmlUtilization_t;

EXPORT int nvmlInit_v2(void) {
  initialized = 1;
  return 0;
}
EXPORT int nvmlShutdown(void) {
  initialized = 0;
  return 0;
}
EXPORT int nvmlDeviceGetCount_v2(unsigned int* n) {
  if (!initialized) return 1; /* NVML_ERROR_UNINITIALIZED */
  *n = 2;
  return 0;
}
/* With FAKE_NVML_LOST set, device 1 has fallen off the bus. */
EXPORT int nvmlDeviceGetHandleByIndex_v2(unsigned int i, fake_device** h) {
  if (i >= 2) return 2; /* NVML_ERROR_INVALID_ARGUMENT */
  if (i == 1 && getenv("FAKE_NVML_LOST")) return 15; /* NVML_ERROR_GPU_IS_LOST */
  *h = &devices[i];
  return 0;
}
EXPORT int nvmlDeviceGetName(fake_device* h, char* buf, unsigned int len) {
  strncpy(buf, h->index == 0 ? "NVIDIA H100 80GB HBM3" : "NVIDIA L4", len - 1);
  return 0;
}
EXPORT int nvmlDeviceGetUUID(fake_device* h, char* buf, unsigned int len) {
  strncpy(buf, h->index == 0 ? "GPU-aaaa" : "GPU-bbbb", len - 1);
  return 0;
}
EXPORT int nvmlDeviceGetMemoryInfo(fake_device* h, nvmlMemory_t* m) {
  m->total = h->index == 0 ? 85899345920ull : 24159191040ull;
  m->used = h->index == 0 ? 42949672960ull : 0;
  m->free = m->total - m->used;
  return 0;
}
/* Like the real driver: the v1 call counts the driver's reserved memory as
 * used, v2 reports it apart. Only device 0 has v2, as if on an old driver. */
EXPORT int nvmlDeviceGetMemoryInfo_v2(fake_device* h, nvmlMemory_v2_t* m) {
  if (m->version != (sizeof(nvmlMemory_v2_t) | (2u << 24))) return 25; /* NVML_ERROR_ARGUMENT_VERSION_MISMATCH */
  if (h->index != 0) return 3;                                         /* NVML_ERROR_NOT_SUPPORTED */
  m->total = 85899345920ull;
  m->reserved = 536870912ull;
  m->used = 42949672960ull - m->reserved;
  m->free = m->total - m->reserved - m->used;
  return 0;
}
EXPORT int nvmlDeviceGetUtilizationRates(fake_device* h, nvmlUtilization_t* u) {
  u->gpu = h->index == 0 ? 87 : 3;
  u->memory = 0;
  return 0;
}
EXPORT int nvmlDeviceGetTemperature(fake_device* h, int sensor, unsigned int* t) {
  if (sensor != 0) return 2;
  *t = h->index == 0 ? 61 : 40;
  return 0;
}
EXPORT int nvmlDeviceGetPowerUsage(fake_device* h, unsigned int* mw) {
  if (h->index == 1) return 3; /* NVML_ERROR_NOT_SUPPORTED */
  *mw = 512500;
  return 0;
}
