#!/bin/sh
# chipagent plugin for Intel Gaudi accelerators, via Habana's hl-smi.
#   chipagent --plugin /opt/chipagent/plugins/gaudi-hl-smi.sh
# Not yet run on real Gaudi hardware. If your hl-smi version names fields
# differently, check `hl-smi --help-query-aip` and adjust QUERY.
set -eu
QUERY=index,name,bus_id,memory.total,memory.used,utilization.aip,temperature.aip,power.draw
hl-smi -Q "$QUERY" -f csv,noheader,nounits | awk -F', *' '
  function num(v) { return (v ~ /^[0-9.]+$/) ? v : "null" }
  BEGIN { printf "{\"devices\":[" }
  NF >= 8 {
    if (n++) printf ","
    mem_total = ($4 ~ /^[0-9.]+$/) ? $4 * 1048576 : 0   # MiB
    mem_used  = ($5 ~ /^[0-9.]+$/) ? $5 * 1048576 : 0
    gsub(/"/, "", $2)
    printf "{\"vendor\":\"habana\",\"kind\":\"accelerator\",\"name\":\"Intel Gaudi %s\",\"id\":\"%s\",", $2, $3
    printf "\"memory_total_bytes\":%.0f,\"memory_used_bytes\":%.0f,", mem_total, mem_used
    printf "\"utilization_percent\":%s,\"temperature_c\":%s,\"power_watts\":%s}", num($6), num($7), num($8)
  }
  END { printf "]}\n" }'
