/* A two-GPU stand-in for libnvidia-ml, exporting the NVML entry points
 * chipagent uses with their real C signatures. */
#include <string.h>

typedef struct {
  int index;
} fake_device;

static fake_device devices[2] = {{0}, {1}};
static int initialized;

typedef struct {
  unsigned long long total, free, used;
} nvmlMemory_t;
typedef struct {
  unsigned int gpu, memory;
} nvmlUtilization_t;

int nvmlInit_v2(void) {
  initialized = 1;
  return 0;
}
int nvmlShutdown(void) {
  initialized = 0;
  return 0;
}
int nvmlDeviceGetCount_v2(unsigned int* n) {
  if (!initialized) return 1; /* NVML_ERROR_UNINITIALIZED */
  *n = 2;
  return 0;
}
int nvmlDeviceGetHandleByIndex_v2(unsigned int i, fake_device** h) {
  if (i >= 2) return 2; /* NVML_ERROR_INVALID_ARGUMENT */
  *h = &devices[i];
  return 0;
}
int nvmlDeviceGetName(fake_device* h, char* buf, unsigned int len) {
  strncpy(buf, h->index == 0 ? "NVIDIA H100 80GB HBM3" : "NVIDIA L4", len - 1);
  return 0;
}
int nvmlDeviceGetUUID(fake_device* h, char* buf, unsigned int len) {
  strncpy(buf, h->index == 0 ? "GPU-aaaa" : "GPU-bbbb", len - 1);
  return 0;
}
int nvmlDeviceGetMemoryInfo(fake_device* h, nvmlMemory_t* m) {
  m->total = h->index == 0 ? 85899345920ull : 24159191040ull;
  m->used = h->index == 0 ? 42949672960ull : 0;
  m->free = m->total - m->used;
  return 0;
}
int nvmlDeviceGetUtilizationRates(fake_device* h, nvmlUtilization_t* u) {
  u->gpu = h->index == 0 ? 87 : 3;
  u->memory = 0;
  return 0;
}
int nvmlDeviceGetTemperature(fake_device* h, int sensor, unsigned int* t) {
  if (sensor != 0) return 2;
  *t = h->index == 0 ? 61 : 40;
  return 0;
}
int nvmlDeviceGetPowerUsage(fake_device* h, unsigned int* mw) {
  if (h->index == 1) return 3; /* NVML_ERROR_NOT_SUPPORTED */
  *mw = 512500;
  return 0;
}
