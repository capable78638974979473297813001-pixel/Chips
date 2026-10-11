#!/usr/bin/env python3
"""chipagent plugin for Intel data center and Arc GPUs, via xpu-smi.

    chipagent --plugin "/opt/chipagent/plugins/intel-xpu-smi.py"

Reports each GPU xpu-smi knows (Data Center GPU Max and Flex, Arc) with
its memory, utilization, temperature and power. chipagent already lists
Intel GPUs from PCI; this plugin adds the readings, merged by PCI address.

Not yet run on real Intel GPU hardware; written against xpu-smi's JSON
output (`discovery -j`, `discovery -d N -j`, `stats -d N -j`). If your
xpu-smi version names fields differently, adjust the keys below.
"""
import json
import subprocess
import sys
from concurrent.futures import ThreadPoolExecutor

TIMEOUT = 4  # seconds per xpu-smi call; chipagent's default plugin timeout is 5


def xpu_smi(*args):
    out = subprocess.run(["xpu-smi", *args, "-j"], capture_output=True, text=True, timeout=TIMEOUT, check=True).stdout
    return json.loads(out)


def number(v):
    """xpu-smi prints some numbers as strings; anything else is unknown."""
    try:
        return float(v)
    except (TypeError, ValueError):
        return None


def stats(device_id):
    """Current readings by metric name, e.g. XPUM_STATS_POWER -> 120.5."""
    try:
        data = xpu_smi("stats", "-d", str(device_id))
    except (OSError, subprocess.SubprocessError, ValueError):
        return {}
    out = {}
    for m in data.get("device_level", []) if isinstance(data, dict) else []:
        if isinstance(m, dict) and "metrics_type" in m:
            out[m["metrics_type"]] = number(m.get("value"))
    return out


def details(device_id):
    try:
        data = xpu_smi("discovery", "-d", str(device_id))
        return data if isinstance(data, dict) else {}
    except (OSError, subprocess.SubprocessError, ValueError):
        return {}


def device(d):
    index = d.get("device_id", 0)
    info, readings = details(index), stats(index)
    total = number(info.get("memory_physical_size_byte")) or 0
    used_mib = readings.get("XPUM_STATS_MEMORY_USED")  # MiB
    return {
        "vendor": "intel",
        "kind": "gpu",
        "name": d.get("device_name") or info.get("device_name") or f"Intel GPU {index}",
        "id": d.get("pci_bdf_address") or info.get("pci_bdf_address") or d.get("uuid") or f"xpu{index}",
        "memory_total_bytes": int(total),
        "memory_used_bytes": int(used_mib * 1048576) if used_mib is not None else 0,
        "utilization_percent": readings.get("XPUM_STATS_GPU_UTILIZATION"),
        "temperature_c": readings.get("XPUM_STATS_GPU_CORE_TEMPERATURE"),
        "power_watts": readings.get("XPUM_STATS_POWER"),
    }


def main():
    try:
        listing = xpu_smi("discovery")
    except (OSError, subprocess.SubprocessError, ValueError) as e:
        print(f"xpu-smi failed: {e}", file=sys.stderr)
        return 1
    gpus = [d for d in listing.get("device_list", []) if isinstance(d, dict)] if isinstance(listing, dict) else []
    # Physical GPUs only: SR-IOV virtual functions would count a card twice.
    gpus = [d for d in gpus if d.get("device_function_type", "physical") == "physical"]
    with ThreadPoolExecutor(max_workers=max(1, len(gpus))) as pool:
        report = list(pool.map(device, gpus))
    json.dump({"devices": report}, sys.stdout)
    print()
    return 0


if __name__ == "__main__":
    sys.exit(main())
