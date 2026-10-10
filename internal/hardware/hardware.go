// Package hardware reads accelerator telemetry from chipagent (see agent/)
// and turns it into routing inputs: which tier a node belongs in and how busy
// its accelerators are, whatever vendor made them.
package hardware

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

// TelemetryPath is where chipagent serves its report.
const TelemetryPath = "/v1/telemetry"

// Device kinds chipagent reports. Display-only adapters (server BMCs, VM
// graphics, pre-compute legacy GPUs) are listed for inventory but are not AI
// capacity.
const (
	KindGPU         = "gpu"
	KindNPU         = "npu"
	KindAccelerator = "accelerator"
	KindDisplay     = "display"
)

// Device is one GPU or accelerator as chipagent reports it. Optional
// readings are nil when the vendor's driver does not provide them.
type Device struct {
	Index              int      `json:"index"`
	Vendor             string   `json:"vendor"`
	Kind               string   `json:"kind"` // empty from chipagent 0.1, which only reported GPUs
	Name               string   `json:"name"`
	ID                 string   `json:"id"`
	Source             string   `json:"source"`
	MemoryTotalBytes   uint64   `json:"memory_total_bytes"`
	MemoryUsedBytes    uint64   `json:"memory_used_bytes"`
	UnifiedMemory      bool     `json:"unified_memory"`
	UtilizationPercent *float64 `json:"utilization_percent"`
	TemperatureC       *float64 `json:"temperature_c"`
	PowerWatts         *float64 `json:"power_watts"`
}

// IsAccelerator reports whether the device can run AI work.
func (d Device) IsAccelerator() bool { return d.Kind != KindDisplay }

// Host describes the machine itself; CPU inference runs on any of them.
type Host struct {
	OS               string   `json:"os"`
	Arch             string   `json:"arch"`
	CPU              string   `json:"cpu"`
	LogicalCPUs      int      `json:"logical_cpus"`
	MemoryTotalBytes uint64   `json:"memory_total_bytes"`
	MemoryUsedBytes  uint64   `json:"memory_used_bytes"`
	CPUFeatures      []string `json:"cpu_features"`
}

// Report is everything one chipagent knows about its machine.
type Report struct {
	Schema   int      `json:"schema"`
	Agent    string   `json:"agent"`
	Version  string   `json:"version"`
	Hostname string   `json:"hostname"`
	Host     *Host    `json:"host"` // nil from chipagent 0.1
	Devices  []Device `json:"devices"`
}

// Accelerators are the devices that count as AI capacity.
func (r *Report) Accelerators() []Device {
	var out []Device
	for _, d := range r.Devices {
		if d.IsAccelerator() {
			out = append(out, d)
		}
	}
	return out
}

// MemoryTotal is the accelerator memory across all accelerators (0 if unknown).
func (r *Report) MemoryTotal() uint64 {
	var n uint64
	for _, d := range r.Accelerators() {
		n += d.MemoryTotalBytes
	}
	return n
}

func (r *Report) MemoryUsed() uint64 {
	var n uint64
	for _, d := range r.Accelerators() {
		n += d.MemoryUsedBytes
	}
	return n
}

// Utilization is the mean busy percentage over accelerators that report one.
func (r *Report) Utilization() (float64, bool) {
	var sum float64
	n := 0
	for _, d := range r.Accelerators() {
		if d.UtilizationPercent != nil {
			sum += *d.UtilizationPercent
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// Vendors lists the distinct accelerator vendors present, sorted.
func (r *Report) Vendors() []string {
	var out []string
	for _, d := range r.Accelerators() {
		if !slices.Contains(out, d.Vendor) {
			out = append(out, d.Vendor)
		}
	}
	slices.Sort(out)
	return out
}

func (r *Report) unified() bool {
	for _, d := range r.Accelerators() {
		if d.UnifiedMemory {
			return true
		}
	}
	return false
}

// Thresholds decide a node's tier from its accelerator memory.
type Thresholds struct {
	HeavyMinBytes    uint64
	StandardMinBytes uint64
}

const gib = 1 << 30

// DefaultThresholds put 64 GiB+ nodes (H100/H200, MI300X, multi-GPU boxes) in
// heavy and 20 GiB+ nodes (L4, A10, RTX 4090, Radeon 7900) in standard.
var DefaultThresholds = Thresholds{HeavyMinBytes: 64 * gib, StandardMinBytes: 20 * gib}

// Tier places a node by its total accelerator memory, which bounds the model
// size and KV cache it can hold:
//   - no accelerators (display adapters don't count): light (CPU inference)
//   - accelerators whose memory the driver does not report: standard
//   - otherwise by HeavyMinBytes and StandardMinBytes
//
// Unified-memory machines (Apple silicon) are capped at standard. They can
// hold large models, but their memory bandwidth is several times lower than
// HBM GPUs, so they generate tokens more slowly.
func (t Thresholds) Tier(r *Report) string {
	if len(r.Accelerators()) == 0 {
		return config.TierLight
	}
	mem := r.MemoryTotal()
	tier := config.TierLight
	switch {
	case mem == 0, mem >= t.StandardMinBytes && mem < t.HeavyMinBytes:
		tier = config.TierStandard
	case mem >= t.HeavyMinBytes:
		tier = config.TierHeavy
	}
	if tier == config.TierHeavy && r.unified() {
		tier = config.TierStandard
	}
	return tier
}

// Fetch reads a report from the chipagent at base.
func Fetch(ctx context.Context, client *http.Client, base *url.URL) (*Report, error) {
	u := *base
	u.Path = strings.TrimSuffix(u.Path, "/") + TelemetryPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chipagent returned %s", resp.Status)
	}
	var r Report
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode chipagent telemetry: %w", err)
	}
	if r.Schema != 1 {
		return nil, fmt.Errorf("unsupported chipagent schema %d", r.Schema)
	}
	return &r, nil
}
