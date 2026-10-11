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

**Both are tested only against sample output of the vendor tools**
(`agent/tests/agent_test.cpp`), not on real Gaudi or Neuron hardware. Run the plugin by hand on
such a machine and compare the output with the vendor tool before relying on it.
