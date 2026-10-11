#!/usr/bin/env python3
"""chipagent plugin for Google Cloud TPUs (v2 to v6e), on TPU VMs.

    chipagent --plugin "/opt/chipagent/plugins/google-tpu.py"

Lists each TPU chip from PCI with its generation and HBM size. When the
`tpu-info` package is installed (pip install tpu-info) and a workload is
running, it adds HBM in use and the duty cycle (the share of time the
chip was busy) from libtpu's runtime metrics. chipagent already lists the
chips from PCI; this plugin adds the readings, merged by PCI address.

Not yet run on real TPU hardware. The PCI IDs and HBM sizes below follow
Google's tpu-info tool; check them against `tpu-info` on your VMs.
"""
import json
import os
import sys

GOOGLE = "0x1ae0"
# PCI device ID (and, for v2/v3, subsystem ID) -> generation, HBM GiB per chip.
CHIPS = {
    ("0x0027", "0x004e"): ("v2", 16),
    ("0x0027", "0x004f"): ("v3", 32),
    ("0x005e", None): ("v4", 32),
    ("0x0063", None): ("v5e", 16),
    ("0x0062", None): ("v5p", 95),
    ("0x006f", None): ("v6e", 32),
}
# chipagent passes its --sysfs-root (the directory holding sys/) to plugins.
ROOT = os.environ.get("CHIPAGENT_SYSFS_ROOT", "/")


def read(path):
    try:
        with open(path) as f:
            return f.read().strip().lower()
    except OSError:
        return ""


def pci_chips():
    """TPU chips on the PCI bus, in address order: (address, generation, hbm GiB)."""
    root = os.path.join(ROOT, "sys", "bus", "pci", "devices")
    try:
        addresses = sorted(os.listdir(root))
    except OSError:
        return []
    chips = []
    for addr in addresses:
        dev = os.path.join(root, addr)
        if read(os.path.join(dev, "vendor")) != GOOGLE:
            continue
        device, subsystem = read(os.path.join(dev, "device")), read(os.path.join(dev, "subsystem_device"))
        chip = CHIPS.get((device, subsystem)) or CHIPS.get((device, None))
        if chip:
            chips.append((addr, *chip))
    return chips


def runtime_usage():
    """Per-chip usage from libtpu's metrics service, via tpu-info; [] when unavailable
    (tpu-info not installed, or no TPU workload running)."""
    try:
        from tpu_info import device, metrics  # type: ignore

        chip_type, count = device.get_local_chips()
        if not chip_type or not count:
            return []
        usage = metrics.get_chip_usage(chip_type)
        return sorted(usage, key=lambda u: u.device_id)
    except Exception as e:  # import errors, gRPC errors: report what PCI shows
        print(f"tpu-info unavailable, reporting without usage: {e}", file=sys.stderr)
        return []


def main():
    chips = pci_chips()
    usage = runtime_usage()
    # v2/v3 chips have two cores, each its own runtime device; sum them per chip.
    per_chip = max(1, len(usage) // len(chips)) if chips and usage and len(usage) % len(chips) == 0 else 0
    report = []
    for i, (addr, gen, hbm_gib) in enumerate(chips):
        d = {
            "vendor": "google",
            "kind": "accelerator",
            "name": f"Google TPU {gen}",
            "id": addr,
            "memory_total_bytes": hbm_gib << 30,
            "memory_used_bytes": 0,
            "utilization_percent": None,
        }
        if per_chip:
            cores = usage[i * per_chip:(i + 1) * per_chip]
            total = sum(int(u.total_memory) for u in cores)
            if total:
                d["memory_total_bytes"] = total
            d["memory_used_bytes"] = sum(int(u.memory_usage) for u in cores)
            d["utilization_percent"] = sum(float(u.duty_cycle_pct) for u in cores) / len(cores)
        report.append(d)
    json.dump({"devices": report}, sys.stdout)
    print()
    return 0


if __name__ == "__main__":
    sys.exit(main())
