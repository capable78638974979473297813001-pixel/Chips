# chipagent plugins

A plugin lets chipagent report hardware it has no built-in support for. It is any command that
prints this machine's devices as JSON:

```json
{"devices": [
  {"vendor": "habana", "kind": "accelerator", "name": "Intel Gaudi HL-225", "id": "0000:19:00.0",
   "memory_total_bytes": 103079215104, "memory_used_bytes": 536870912,
   "utilization_percent": 37, "temperature_c": 41, "power_watts": 180}
]}
```

- **Required:** `vendor` and `name`.
- **`kind`:** one of `gpu`, `npu`, `accelerator` or `display`. The default is `accelerator`.
- **Everything else:** optional. Omit a reading, or set it to `null`, when the tool doesn't provide it.
- **Format:** a bare array of device objects also works.

Run it with `chipagent --plugin "<command>"`; `--plugin` can be repeated. The command runs through the
shell on every telemetry request and must finish within `--plugin-timeout` (default 5000 ms), or it
is killed and its devices are left out of that report. A failing plugin is logged and never stops
the agent.

| Plugin | Hardware | Needs | Reports |
|---|---|---|---|
| `gaudi-hl-smi.sh` | Intel Gaudi (Habana) | `hl-smi` | name, PCI address, memory, utilization, temperature, power |
| `aws-neuron.py` | AWS Inferentia / Trainium | `neuron-ls`, Python 3 | device, NeuronCore count, memory |
| `intel-xpu-smi.py` | Intel Data Center GPU Max and Flex, and other GPUs `xpu-smi` supports | `xpu-smi`, Python 3 | name, PCI address, memory total and used, utilization, temperature, power |
| `google-tpu.py` | Google Cloud TPU v2 to v6e, on TPU VMs | Python 3; `tpu-info` (pip) for usage | generation, PCI address, HBM; HBM used and duty cycle while a workload runs |

**Devices are merged by PCI address.** chipagent already lists every PCI accelerator itself. When
a plugin reports the same PCI address, its readings fill in that device instead of adding a second
one, so a fleet's accelerator count stays right. Report `id` as the PCI address (`0000:3b:00.0`, or
`3b:00.0`) whenever the vendor tool gives one. Plugins that read `/sys` themselves should honor
`CHIPAGENT_SYSFS_ROOT`, which chipagent sets to its `--sysfs-root`.

**All four are tested only against sample output of the vendor tools**
(`agent/tests/agent_test.cpp`), not on real Gaudi, Neuron, Intel GPU or TPU hardware. The TPU
plugin's usage readings come from the `tpu-info` package's Python API
(`device.get_local_chips`, `metrics.get_chip_usage`), which the tests stand in for. Run a plugin by
hand on such a machine and compare its output with the vendor tool before relying on it.
