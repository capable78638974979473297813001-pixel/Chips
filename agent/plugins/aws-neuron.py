#!/usr/bin/env python3
"""chipagent plugin for AWS Inferentia and Trainium, via neuron-ls.

    chipagent --plugin "/opt/chipagent/plugins/aws-neuron.py"

Reports each Neuron device with its memory and NeuronCore count. Neuron
utilization needs neuron-monitor and is not read here. Not yet run on real
Inferentia/Trainium hardware: if your neuron-ls version names fields
differently, adjust the keys below.
"""
import json
import subprocess
import sys

try:
    out = subprocess.run(["neuron-ls", "--json-output"], capture_output=True, text=True, timeout=10, check=True).stdout
    devices = json.loads(out)
except (OSError, subprocess.SubprocessError, json.JSONDecodeError) as e:
    print(f"neuron-ls failed: {e}", file=sys.stderr)
    sys.exit(1)

report = []
for d in devices if isinstance(devices, list) else []:
    index = d.get("neuron_device", len(report))
    cores = d.get("nc_count")
    name = f"AWS Neuron device {index}" + (f" ({cores} NeuronCores)" if cores else "")
    report.append({
        "vendor": "aws",
        "kind": "accelerator",
        "name": name,
        "id": d.get("bdf", f"neuron{index}"),
        "memory_total_bytes": int(d.get("memory_size", 0) or 0),
    })
json.dump({"devices": report}, sys.stdout)
print()
