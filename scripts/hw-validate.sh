#!/usr/bin/env bash
# Real-hardware validation for chipagent and SiliconRouter on a rented GPU
# server (RunPod or any machine running a vLLM image, CUDA or ROCm). It runs
# a fixed set of checks, writes the results to $OUT, and serves that
# directory read-only on :8888 so they can be fetched over HTTP.
#
#   VLLM_API_KEY=<random> SUITE=core,repeat scripts/hw-validate.sh
#
# Two vLLM engines run behind the router: one per GPU when the machine has
# two or more, else both on the one GPU. SUITE picks what runs, in order:
#   core      chipagent's tests and readings next to the vendor tool's
#             (nvidia-smi, rocm-smi) and raw sysfs, idle and under load;
#             router overhead; prefix-cache routing on and off; routing
#             while one engine is flooded directly, bypassing the router.
#   repeat    prefix-cache routing on and off, REPEATS times each,
#             alternating, for run-to-run variance.
#   failover  engine B killed (SIGKILL) under streaming load, then restarted.
#   stall     engine B's engine core frozen (SIGSTOP) under streaming load:
#             its API server still answers health checks, so only stall
#             detection can see it. Resumed after 45 s.
#   soak      SOAK_MIN minutes of load from two teams, sampling the router's
#             goroutines and heap, then the cost report checked against
#             the clock (POD_PRICE_PER_HOUR).
#   lora      engines restarted with LoRA slots; the adapters in
#             LORA_ADAPTERS served with LoRA-aware routing, then with every
#             adapter on both engines (routing on load alone).
# vLLM instance A stays up afterwards (port 8001, needs VLLM_API_KEY), with
# chipagent on 9400, for tests from outside. The machine shuts itself down
# after TTL_MIN minutes where the platform allows it.
set -uo pipefail
OUT=${OUT:-/results}
MODEL=${MODEL:-Qwen/Qwen2.5-1.5B-Instruct}
SUITE=${SUITE:-core}
TTL_MIN=${TTL_MIN:-120}
REPEATS=${REPEATS:-3}
SOAK_MIN=${SOAK_MIN:-20}
POD_PRICE_PER_HOUR=${POD_PRICE_PER_HOUR:-1.00}
MODEL_GIB=${MODEL_GIB:-3} # the model's weights
MAX_MODEL_LEN=${MAX_MODEL_LEN:-16384}
# Hugging Face repos of LoRA adapters for MODEL (these fit Qwen2.5-7B-Instruct).
LORA_ADAPTERS=${LORA_ADAPTERS:-zjudai/flowertune-medical-lora-qwen2.5-7b-instruct,Tamir39/qwen2_5-7b-vietnam-tax-lora,millat/Qwen2.5-7B-BDLAW-LoRA,Manikanta23/qwen2.5-7b-rtl-vlsi-lora,durganani60/qwen2.5-7b-pii-financial,Tomas0626/amparo-lora-qwen2.5-7b,chchen/Qwen2.5-7B-Instruct-PsyCourse-fold6,chchen/Qwen2.5-7B-Instruct-PsyCourse-fold3}
GO_VERSION=1.24.7
: "${VLLM_API_KEY:?set VLLM_API_KEY}"
export VLLM_API_KEY
mkdir -p "$OUT"
cd "$(dirname "$0")/.."

log() { echo "[$(date -u +%H:%M:%S)] $*" | tee -a "$OUT/progress.log"; }
# step NAME CMD...: runs CMD, saving its output and exit status to NAME.txt.
step() {
  local name=$1; shift
  log "$name"
  "$@" > "$OUT/$name.txt" 2>&1
  echo "exit status $?" >> "$OUT/$name.txt"
}
has() { [[ ",$SUITE," == *",$1,"* ]]; }
python3 -m http.server 8888 --directory "$OUT" > /dev/null 2>&1 &

# Backstop: remove this pod after TTL_MIN even if nobody stops it.
(
  sleep $((TTL_MIN * 60))
  log "time limit reached; shutting down"
  if [ -n "${RUNPOD_POD_ID:-}" ] && [ -n "${RUNPOD_API_KEY:-}" ]; then
    curl -s -H 'Content-Type: application/json' -H "Authorization: Bearer $RUNPOD_API_KEY" \
      -d "{\"query\":\"mutation { podTerminate(input: {podId: \\\"$RUNPOD_POD_ID\\\"}) }\"}" \
      https://api.runpod.io/graphql
  fi
  kill 1
) &

log "installing build tools (suite: $SUITE, model: $MODEL)"
if [ -z "${SKIP_INSTALL:-}" ]; then # set it to try the script on a machine already set up
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq > /dev/null 2>&1
  apt-get install -y -qq build-essential cmake pciutils curl > /dev/null 2>&1
  curl -sSL "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz" | tar -C /usr/local -xz
  export PATH=/usr/local/go/bin:$PATH
fi

if command -v nvidia-smi > /dev/null; then
  VENDOR=nvidia; NGPU=$(nvidia-smi -L | grep -c '^GPU')
elif command -v rocm-smi > /dev/null || command -v amd-smi > /dev/null; then
  VENDOR=amd; NGPU=$(ls -d /dev/dri/renderD* 2> /dev/null | wc -l)
else VENDOR=unknown; NGPU=1; fi
# Fraction of GPU memory per engine; lower it to put the KV cache under
# pressure, where prefix-cache placement matters most.
# With KV_GIB set, size it so each engine's KV cache gets about that much
# whatever the GPU: MODEL_GIB of weights plus 2 GiB for activations.
if [ -n "${KV_GIB:-}" ] && [ "$VENDOR" = nvidia ]; then
  TOTAL_MIB=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits -i 0)
  MEM_UTIL=$(python3 -c "print(round(min(0.92, ($MODEL_GIB + 2 + $KV_GIB) * 1024 / $TOTAL_MIB), 3))")
fi
if [ "$NGPU" -ge 2 ]; then MEM_UTIL=${MEM_UTIL:-0.85}; else MEM_UTIL=${MEM_UTIL:-0.35}; fi
echo "$VENDOR $NGPU" > "$OUT/vendor.txt"

step build-agent make agent
step build-go go build -o bin/ ./cmd/...
step chipagent-tests agent/build/chipagent_test

vendor_readings() { # $1: label
  if [ "$VENDOR" = nvidia ]; then
    nvidia-smi --query-gpu=index,name,pci.bus_id,memory.total,memory.used,utilization.gpu,temperature.gpu,power.draw \
      --format=csv > "$OUT/vendor-$1.csv" 2>&1
  else
    rocm-smi --showproductname --showbus --showmeminfo vram --showuse --showtemp --showpower --json \
      > "$OUT/vendor-$1.json" 2>&1
    command -v amd-smi > /dev/null && amd-smi metric --json > "$OUT/amd-smi-$1.json" 2>&1
  fi
  agent/build/chipagent --once > "$OUT/chipagent-$1.json" 2> "$OUT/chipagent-$1.err"
}
log "readings while idle"
vendor_readings idle
if [ "$VENDOR" = nvidia ]; then
  nvidia-smi -q > "$OUT/nvidia-smi-q.txt" 2>&1
  nvidia-smi topo -m > "$OUT/topology.txt" 2>&1
else
  rocm-smi --showtopo > "$OUT/topology.txt" 2>&1
fi
lspci -nn > "$OUT/lspci.txt" 2>&1
ls -l /dev/dri /dev/accel /dev/kfd /dev/nvidia* > "$OUT/dev-nodes.txt" 2>&1
for d in /sys/class/drm/card*/device; do
  echo "== $d"
  for f in vendor device class mem_info_vram_total mem_info_vram_used gpu_busy_percent; do
    printf '%s=%s\n' "$f" "$(cat "$d/$f" 2> /dev/null)"
  done
  ls "$d"/hwmon/*/ 2> /dev/null | tr '\n' ' '; echo
done > "$OUT/sysfs-drm.txt" 2>&1

agent/build/chipagent --listen :9400 > "$OUT/chipagent-server.log" 2>&1 &

# Each engine runs in its own process group (setsid), so it can be killed
# or frozen whole. Each is limited to 16 concurrent sequences so a flood
# fills its queue. With two or more GPUs, engine A gets GPU 0 and B GPU 1.
declare -A ENGINE
LORA_FLAGS=()
start_vllm() { # $1: port
  local port=$1 gpu=0
  [ "$port" = 8002 ] && [ "$NGPU" -ge 2 ] && gpu=1
  log "starting vLLM on :$port (GPU $gpu)"
  CUDA_DEVICE_ORDER=PCI_BUS_ID CUDA_VISIBLE_DEVICES=$gpu HIP_VISIBLE_DEVICES=$gpu VLLM_ALLOW_RUNTIME_LORA_UPDATING=True \
    setsid vllm serve "$MODEL" --port "$port" --api-key "$VLLM_API_KEY" --gpu-memory-utilization "$MEM_UTIL" \
    --max-model-len "$MAX_MODEL_LEN" --max-num-seqs 16 --enable-prefix-caching --enable-prompt-tokens-details \
    "${LORA_FLAGS[@]}" >> "$OUT/vllm-$port.log" 2>&1 &
  ENGINE[$port]=$!
  for _ in $(seq 1 240); do
    curl -sf "localhost:$port/health" > /dev/null && { log "vLLM :$port ready"; return 0; }
    sleep 5
  done
  log "vLLM :$port did not start"; return 1
}
stop_vllm() { # $1: port
  kill -9 -- "-${ENGINE[$1]}" 2> /dev/null
  wait "${ENGINE[$1]}" 2> /dev/null
}
# The engine-core process of an engine: everything in its group but the
# API server.
engine_core() { ps -eo pid,pgid | awk -v g="${ENGINE[$1]}" '$2 == g && $1 != g { print $1 }'; }
start_vllm 8001 && start_vllm 8002 || { log "FAILED: vLLM"; touch "$OUT/DONE"; wait; }
curl -s localhost:8001/metrics > "$OUT/vllm-metrics.txt"
curl -s localhost:8001/version > "$OUT/vllm-version.txt"

# router_config AFFINITY [one] [extra YAML...]
router_config() {
  local affinity=$1 one=${2:-} dev_a= dev_b=; shift 2 2> /dev/null || shift $#
  # One engine per GPU: each backend counts only its own GPU.
  [ "$NGPU" -ge 2 ] && dev_a='agent_devices: ["0"],' dev_b='agent_devices: ["1"],'

  cat << EOF
listen: "127.0.0.1:8080"
backends:
  - {name: gpu-a, url: "http://127.0.0.1:8001", api_key_env: VLLM_API_KEY, metrics_path: /metrics,
     agent_url: "http://127.0.0.1:9400", $dev_a tier: auto, max_concurrency: 32, ${BACKEND_EXTRA:-}}
EOF
  [ "$one" = one ] || cat << EOF
  - {name: gpu-b, url: "http://127.0.0.1:8002", api_key_env: VLLM_API_KEY, metrics_path: /metrics,
     agent_url: "http://127.0.0.1:9400", $dev_b tier: auto, max_concurrency: 32, ${BACKEND_EXTRA:-}}
EOF
  echo "routing: {prefix_affinity: $affinity}"
  echo "engine_metrics: {interval: 1s}"
  echo "health: {interval: 1s}"
  for extra in "$@"; do echo "$extra"; done
}
ROUTER=
run_router() { # $1: prefix affinity, $2: label, $3: "one" for gpu-a only, more YAML lines follow
  local affinity=$1 label=$2 one=${3:-}; shift 3 2> /dev/null || shift $#
  stop_router
  router_config "$affinity" "$one" "$@" > "$OUT/router-$label.yaml"
  bin/siliconrouter -config "$OUT/router-$label.yaml" > "$OUT/router-$label.log" 2>&1 &
  ROUTER=$!
  for _ in $(seq 1 30); do curl -sf localhost:8080/readyz > /dev/null && break; sleep 1; done
  sleep 3 # a few engine scrapes and a chipagent poll
}
stop_router() { [ -n "$ROUTER" ] && { kill "$ROUTER"; wait "$ROUTER" 2> /dev/null; ROUTER=; }; }
# vLLM's own prefix-cache counters, summed over both engines.
cache_counters() {
  for port in 8001 8002; do curl -s "localhost:$port/metrics"; done |
    awk '/^vllm:prefix_cache_(queries|hits)_total/ { split($1, n, "{"); sum[n[1]] += $2 }
         END { printf "%d %d\n", sum["vllm:prefix_cache_queries_total"], sum["vllm:prefix_cache_hits_total"] }'
}
bench() { # $1: label, $2: url, more srbench flags follow
  local label=$1 url=$2; shift 2
  read -r q0 h0 < <(cache_counters)
  step "bench-$label" bin/srbench -url "$url" -api-key "${BENCH_KEY:-$VLLM_API_KEY}" -model "$MODEL" "$@"
  read -r q1 h1 < <(cache_counters)
  echo "engine prefix cache: $((h1 - h0)) of $((q1 - q0)) prompt tokens hit" >> "$OUT/bench-$label.txt"
}
# timeline LABEL: until stopped, one line a second with each backend's
# router-side state.
timeline() {
  while :; do
    printf '%s ' "$(date -u +%H:%M:%S)"
    curl -s -m 1 localhost:8080/admin/backends | python3 -c '
import json, sys
try:
    for b in json.load(sys.stdin)["backends"]:
        e = b.get("engine") or {}
        print("%s healthy=%s inflight=%s running=%s waiting=%s stalled=%s" % (b["name"], b.get("healthy"),
              b.get("inflight"), e.get("running"), e.get("waiting"), e.get("stalled", False)), end="  ")
except Exception as err:
    print("admin unreachable:", err, end="")
print()'
    sleep 1
  done > "$OUT/timeline-$1.txt" 2>&1
}
# Long shared system prompts make prefix-cache placement matter.
# BENCH_ARGS overrides it (word-split), e.g. long unique histories.
if [ -n "${BENCH_ARGS:-}" ]; then read -r -a BENCH <<< "$BENCH_ARGS"
else BENCH=(-conversations 48 -concurrency 16 -turns 6 -system-chars 24000 -user-chars 800); fi
URL=http://127.0.0.1:8080/v1/chat/completions

# Warm both engines up first (compilation, graph capture, allocator), so
# the first measured run is not charged for it.
bench warmup-a http://127.0.0.1:8001/v1/chat/completions -conversations 16 -concurrency 16 -turns 2
bench warmup-b http://127.0.0.1:8002/v1/chat/completions -conversations 16 -concurrency 16 -turns 2

if has core; then
  # Router overhead: the same engine directly and through the router.
  bench direct-one-engine http://127.0.0.1:8001/v1/chat/completions "${BENCH[@]}"
  run_router true one-backend one
  bench router-one-backend "$URL" "${BENCH[@]}"

  run_router true affinity
  curl -s localhost:8080/admin/backends > "$OUT/backends.json"
  bench router-affinity "$URL" "${BENCH[@]}"
  curl -s localhost:8080/metrics > "$OUT/router-metrics-after-bench.txt"

  log "flooding gpu-a directly, bypassing the router"
  # 48 requests forced to generate 3000 tokens each (ignore_eos) against 16
  # sequence slots: the engine queues the rest.
  FLOOD=()
  for i in $(seq 1 48); do
    curl -s -o /dev/null -H "Authorization: Bearer $VLLM_API_KEY" -H 'Content-Type: application/json' \
      -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"flood $i\"}],\"max_tokens\":3000,\"ignore_eos\":true}" \
      localhost:8001/v1/chat/completions &
    FLOOD+=($!)
  done
  sleep 8
  vendor_readings load
  curl -s localhost:8001/metrics | grep -E '^vllm:(num_requests|kv_cache|gpu_cache)' > "$OUT/vllm-metrics-flooded.txt"
  curl -s localhost:8080/admin/backends > "$OUT/backends-flooded.json"
  for i in $(seq 1 20); do
    curl -s -o /dev/null -D - -H 'Content-Type: application/json' \
      -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"probe $i\"}],\"max_tokens\":4}" \
      localhost:8080/v1/chat/completions | tr -d '\r' | grep -i '^x-siliconrouter-backend'
  done | sort | uniq -c > "$OUT/routing-while-flooded.txt"
  curl -s localhost:8080/metrics | grep engine_ > "$OUT/router-engine-gauges-flooded.txt"
  kill "${FLOOD[@]}" 2> /dev/null; wait "${FLOOD[@]}" 2> /dev/null
  sleep 10 # let gpu-a drain the cancelled requests

  run_router false no-affinity
  bench router-no-affinity "$URL" "${BENCH[@]}"
fi

if has repeat; then
  for i in $(seq 1 "$REPEATS"); do
    for affinity in true false; do
      run_router "$affinity" "repeat-$affinity-$i"
      bench "repeat-$affinity-$i" "$URL" "${BENCH[@]}"
    done
  done
fi

# Streaming load for the failure tests: 64 conversations of 4 turns, each
# reply 256 tokens, so engine B always has requests mid-stream.
FAULT_BENCH=(-conversations 64 -concurrency 16 -turns 4 -system-chars 2000 -user-chars 400 -max-tokens 256 -stream -timeout 120s)

if has failover; then
  run_router false failover "" "retry: {first_token_timeout: 10s}"
  timeline failover & TL=$!
  bench failover-baseline "$URL" "${FAULT_BENCH[@]}"
  ( sleep 15; log "killing engine B (SIGKILL)"; stop_vllm 8002 ) & KILLER=$!
  bench failover "$URL" "${FAULT_BENCH[@]}"
  wait "$KILLER"
  log "restarting engine B"
  T0=$(date +%s)
  start_vllm 8002
  for _ in $(seq 1 60); do
    curl -s localhost:8080/admin/backends | grep -q '"name":"gpu-b"[^}]*"healthy":true' && break
    sleep 1
  done
  echo "engine B: restarted and back in rotation after $(($(date +%s) - T0)) s" >> "$OUT/bench-failover.txt"
  bench failover-after "$URL" -conversations 16 -concurrency 16 -turns 2 -max-tokens 32
  kill "$TL"
  grep -iE 'unhealthy|healthy|retry|error' "$OUT/router-failover.log" | tail -50 > "$OUT/failover-router-events.txt"
fi

if has stall; then
  run_router false stall "" "retry: {first_token_timeout: 10s}" "upstream: {response_header_timeout: 60s}"
  timeline stall & TL=$!
  (
    sleep 15
    CORE=$(engine_core 8002)
    log "freezing engine B's engine core (SIGSTOP $CORE)"
    kill -STOP $CORE
    sleep 45
    log "resuming engine B (SIGCONT)"
    kill -CONT $CORE
  ) &
  bench stall "$URL" "${FAULT_BENCH[@]}"
  kill "$TL"
  grep -iE 'stall|retry|unhealthy' "$OUT/router-stall.log" | tail -50 > "$OUT/stall-router-events.txt"
  curl -s localhost:8080/metrics | grep -E 'stall|retr' > "$OUT/stall-router-metrics.txt"
fi

if has soak; then
  export TEAM_A_KEY=team-a-$RANDOM$RANDOM TEAM_B_KEY=team-b-$RANDOM$RANDOM ADMIN_KEY=admin-$RANDOM$RANDOM
  HALF=$(python3 -c "print($POD_PRICE_PER_HOUR / 2)")
  BACKEND_EXTRA="cost_per_hour: $HALF" run_router true soak "" \
    "auth: {admin_keys_env: ADMIN_KEY, keys: [{name: team-a, key_env: TEAM_A_KEY}, {name: team-b, key_env: TEAM_B_KEY}]}"
  T0=$(date +%s.%N)
  END=$(($(date +%s) + SOAK_MIN * 60))
  log "soak for $SOAK_MIN minutes"
  soak_load() { # $1: team, $2: key, $3: concurrency
    while [ "$(date +%s)" -lt "$END" ]; do
      bin/srbench -url "$URL" -api-key "$2" -model "$MODEL" -conversations $(($3 * 2)) -concurrency "$3" \
        -turns 4 -system-chars 6000 -user-chars 600 -max-tokens 96 2>&1 | sed "s/^/$(date -u +%H:%M:%S) $1 /"
    done >> "$OUT/soak-bench.txt"
  }
  soak_load team-a "$TEAM_A_KEY" 12 & L1=$!
  soak_load team-b "$TEAM_B_KEY" 4 & L2=$!
  while [ "$(date +%s)" -lt "$END" ]; do
    curl -s localhost:8080/metrics | awk -v t="$(date -u +%H:%M:%S)" '
      $1 == "siliconrouter_goroutines" { g = $2 } $1 == "siliconrouter_heap_bytes" { h = $2 }
      END { printf "%s goroutines %d heap_mib %.1f\n", t, g, h / 1048576 }' >> "$OUT/soak-samples.txt"
    sleep 30
  done
  wait "$L1" "$L2"
  sleep 5
  T1=$(date +%s.%N)
  curl -s localhost:8080/metrics >> "$OUT/soak-router-metrics.txt"
  curl -s -H "Authorization: Bearer $ADMIN_KEY" localhost:8080/admin/costs > "$OUT/soak-costs.json"
  python3 - "$OUT/soak-costs.json" "$T0" "$T1" "$POD_PRICE_PER_HOUR" > "$OUT/soak-costs-check.txt" << 'PYEOF'
import json, sys
c = json.load(open(sys.argv[1]))
hours = (float(sys.argv[3]) - float(sys.argv[2])) / 3600
expect = hours * float(sys.argv[4])
print("report:", json.dumps(c, indent=1))
print("router up for the soak: %.4f h at $%s/h = $%.4f (plus the minutes before the soak began)" % (hours, sys.argv[4], expect))
print("reported total: $%.4f, of which idle $%.4f" % (c.get("total_dollars", 0), c.get("idle_dollars", 0)))
PYEOF
  unset BACKEND_EXTRA
fi

if has lora; then
  log "restarting both engines with LoRA slots"
  stop_router; stop_vllm 8001; stop_vllm 8002
  # Each engine holds 4 adapters on the GPU at once; 8 adapters overflow one
  # engine but fit across both, if each adapter stays on one engine.
  LORA_FLAGS=(--enable-lora --max-loras 4 --max-lora-rank 32 --max-cpu-loras 8)
  ADAPTERS=() NAMES=()
  python3 -m pip install -q huggingface_hub > /dev/null 2>&1
  for repo in ${LORA_ADAPTERS//,/ }; do
    name=$(basename "$repo" | tr 'A-Z_.' 'a-z--')
    python3 -c "from huggingface_hub import snapshot_download as d; d('$repo', local_dir='/adapters/$name')" \
      >> "$OUT/lora-download.log" 2>&1 || { log "could not download $repo"; continue; }
    ADAPTERS+=("{name: $name, base: \"$MODEL\", path: /adapters/$name}") NAMES+=("$name")
  done
  start_vllm 8001 && start_vllm 8002
  MODELS=$(IFS=,; echo "${NAMES[*]}")
  ADAPTER_YAML="models: {adapters: [$(IFS=,; echo "${ADAPTERS[*]}")]}"
  LORA_BENCH=(-models "$MODELS" -conversations 64 -concurrency 24 -turns 4 -system-chars 2000 -user-chars 400 -max-tokens 64)
  BACKEND_EXTRA="dynamic_lora: true" run_router false lora-aware "" "$ADAPTER_YAML"
  bench lora-warmup "$URL" -models "$MODELS" -conversations 16 -concurrency 16 -turns 1
  bench lora-aware "$URL" "${LORA_BENCH[@]}"
  curl -s localhost:8080/admin/backends > "$OUT/lora-aware-backends.json"
  curl -s localhost:8080/metrics | grep lora > "$OUT/lora-aware-router-metrics.txt"
  for port in 8001 8002; do curl -s -H "Authorization: Bearer $VLLM_API_KEY" "localhost:$port/v1/models"; echo; done > "$OUT/lora-aware-models.txt"

  # Load-only: every adapter loaded on both engines, so the router sees
  # every adapter everywhere and routes on load alone.
  for port in 8001 8002; do
    for name in "${NAMES[@]}"; do
      curl -s -H "Authorization: Bearer $VLLM_API_KEY" -H 'Content-Type: application/json' \
        -d "{\"lora_name\":\"$name\",\"lora_path\":\"/adapters/$name\"}" "localhost:$port/v1/load_lora_adapter"; echo
    done
  done > "$OUT/lora-load-all.txt"
  BACKEND_EXTRA="dynamic_lora: true" run_router false lora-everywhere "" "$ADAPTER_YAML"
  bench lora-everywhere "$URL" "${LORA_BENCH[@]}"
  curl -s localhost:8080/metrics | grep lora > "$OUT/lora-everywhere-router-metrics.txt"
  # And once more LoRA-aware, now that every adapter is everywhere: the
  # router keeps each adapter where its requests already run.
  bench lora-everywhere-2 "$URL" "${LORA_BENCH[@]}"
  unset BACKEND_EXTRA
fi

stop_router
stop_vllm 8002 # keep instance A for tests from outside
log "done"
touch "$OUT/DONE"
wait
