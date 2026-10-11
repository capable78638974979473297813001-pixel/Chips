#!/usr/bin/env bash
# Real-hardware validation for chipagent and SiliconRouter on a rented GPU
# server (RunPod or any machine running a vLLM image, CUDA or ROCm). It runs
# a fixed set of checks, writes the results to $OUT, and serves that
# directory read-only on :8888 so they can be fetched over HTTP.
#
#   VLLM_API_KEY=<random> scripts/hw-validate.sh
#
# What it records:
#   - chipagent's test suite and its readings, next to the vendor tool's
#     (nvidia-smi or rocm-smi) and raw sysfs, idle and under load;
#   - two vLLM instances on the GPU behind the router: /admin/backends with
#     chipagent hardware and engine metrics, and the raw vLLM /metrics;
#   - srbench through the router with and without prefix-cache routing;
#   - routing while one engine is flooded directly, bypassing the router.
# vLLM instance A stays up afterwards (port 8001, needs VLLM_API_KEY), with
# chipagent on 9400, for tests from outside. The machine shuts itself down
# after TTL_MIN minutes where the platform allows it.
set -uo pipefail
OUT=${OUT:-/results}
MODEL=${MODEL:-Qwen/Qwen2.5-1.5B-Instruct}
TTL_MIN=${TTL_MIN:-120}
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

log "installing build tools"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq > /dev/null 2>&1
apt-get install -y -qq build-essential cmake pciutils curl > /dev/null 2>&1
curl -sSL "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz" | tar -C /usr/local -xz
export PATH=/usr/local/go/bin:$PATH

if command -v nvidia-smi > /dev/null; then VENDOR=nvidia
elif command -v rocm-smi > /dev/null || command -v amd-smi > /dev/null; then VENDOR=amd
else VENDOR=unknown; fi
echo "$VENDOR" > "$OUT/vendor.txt"

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
[ "$VENDOR" = nvidia ] && nvidia-smi -q > "$OUT/nvidia-smi-q.txt" 2>&1
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

# Two engines on the one GPU, each limited to 16 concurrent sequences so a
# flood fills its queue.
start_vllm() { # $1: port
  log "starting vLLM on :$1"
  vllm serve "$MODEL" --port "$1" --api-key "$VLLM_API_KEY" --gpu-memory-utilization 0.35 \
    --max-model-len 16384 --max-num-seqs 16 --enable-prefix-caching --enable-prompt-tokens-details \
    > "$OUT/vllm-$1.log" 2>&1 &
  for _ in $(seq 1 240); do
    curl -sf "localhost:$1/health" > /dev/null && { log "vLLM :$1 ready"; return 0; }
    sleep 5
  done
  log "vLLM :$1 did not start"; return 1
}
start_vllm 8001 && start_vllm 8002 || { log "FAILED: vLLM"; touch "$OUT/DONE"; wait; }
curl -s localhost:8001/metrics > "$OUT/vllm-metrics.txt"
curl -s localhost:8001/version > "$OUT/vllm-version.txt"

router_config() { # $1: prefix affinity true/false, $2: "one" for gpu-a only
  cat << EOF
listen: "127.0.0.1:8080"
backends:
  - {name: gpu-a, url: "http://127.0.0.1:8001", api_key_env: VLLM_API_KEY, metrics_path: /metrics,
     agent_url: "http://127.0.0.1:9400", tier: auto, max_concurrency: 32}
EOF
  [ "${2:-}" = one ] || cat << EOF
  - {name: gpu-b, url: "http://127.0.0.1:8002", api_key_env: VLLM_API_KEY, metrics_path: /metrics,
     agent_url: "http://127.0.0.1:9400", tier: auto, max_concurrency: 32}
EOF
  cat << EOF
routing: {prefix_affinity: $1}
engine_metrics: {interval: 1s}
EOF
}
run_router() { # $1: prefix affinity, $2: label, $3: "one" for gpu-a only
  router_config "$1" "${3:-}" > "$OUT/router-$2.yaml"
  bin/siliconrouter -config "$OUT/router-$2.yaml" > "$OUT/router-$2.log" 2>&1 &
  ROUTER=$!
  for _ in $(seq 1 30); do curl -sf localhost:8080/readyz > /dev/null && break; sleep 1; done
  sleep 3 # a few engine scrapes and a chipagent poll
}
# vLLM's own prefix-cache counters, summed over both engines.
cache_counters() {
  for port in 8001 8002; do curl -s "localhost:$port/metrics"; done |
    awk '/^vllm:prefix_cache_(queries|hits)_total/ { split($1, n, "{"); sum[n[1]] += $2 }
         END { printf "%d %d\n", sum["vllm:prefix_cache_queries_total"], sum["vllm:prefix_cache_hits_total"] }'
}
bench() { # $1: label, $2: url, more srbench flags follow
  local label=$1 url=$2; shift 2
  read -r q0 h0 < <(cache_counters)
  step "bench-$label" bin/srbench -url "$url" -api-key "$VLLM_API_KEY" -model "$MODEL" "$@"
  read -r q1 h1 < <(cache_counters)
  echo "engine prefix cache: $((h1 - h0)) of $((q1 - q0)) prompt tokens hit" >> "$OUT/bench-$label.txt"
}
# Long shared system prompts make prefix-cache placement matter.
BENCH=(-conversations 48 -concurrency 16 -turns 6 -system-chars 24000 -user-chars 800)

# Warm both engines up first (compilation, graph capture, allocator), so
# the first measured run is not charged for it.
bench warmup-a http://127.0.0.1:8001/v1/chat/completions -conversations 16 -concurrency 16 -turns 2
bench warmup-b http://127.0.0.1:8002/v1/chat/completions -conversations 16 -concurrency 16 -turns 2
# Router overhead: the same engine directly and through the router.
bench direct-one-engine http://127.0.0.1:8001/v1/chat/completions "${BENCH[@]}"
run_router true one-backend one
bench router-one-backend http://127.0.0.1:8080/v1/chat/completions "${BENCH[@]}"
kill "$ROUTER"; wait "$ROUTER" 2> /dev/null

run_router true affinity
curl -s localhost:8080/admin/backends > "$OUT/backends.json"
bench router-affinity http://127.0.0.1:8080/v1/chat/completions "${BENCH[@]}"
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

kill "$ROUTER"; wait "$ROUTER" 2> /dev/null
run_router false no-affinity
bench router-no-affinity http://127.0.0.1:8080/v1/chat/completions "${BENCH[@]}"
kill "$ROUTER"; wait "$ROUTER" 2> /dev/null

pkill -f "port 8002" # keep instance A for tests from outside
log "done"
touch "$OUT/DONE"
wait
