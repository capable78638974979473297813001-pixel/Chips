#!/usr/bin/env bash
# Soak test: runs the router under steady load for DURATION seconds while
# things go wrong, then checks that it held up and did not leak.
#   - three simulated backends, one failing 10% of requests;
#   - one backend killed and restarted mid-run;
#   - two config reloads (SIGHUP) mid-run.
# It samples the router's goroutines and heap every 5 s and fails if they
# grow steadily, if slots stay held at the end, or if the router stops
# serving. Needs `make build` first.
#   DURATION=300 scripts/soak.sh
set -uo pipefail
cd "$(dirname "$0")/.."
DURATION=${DURATION:-120}
WORK=$(mktemp -d)
PORT=19580
trap 'kill $(jobs -p) 2> /dev/null; wait 2> /dev/null; rm -rf "$WORK"' EXIT

backend() { # port name extra-flags...
  local port=$1 name=$2; shift 2
  bin/mockllm -addr "127.0.0.1:$port" -name "$name" -model llama -decode-tps 400 -max-concurrency 8 "$@" \
    >> "$WORK/$name.log" 2>&1 &
}
backend 19501 steady
backend 19502 flaky -fail-rate 0.1
backend 19503 victim
VICTIM=$!

cat > "$WORK/router.yaml" << EOF
listen: "127.0.0.1:$PORT"
backends:
  - {name: steady, url: "http://127.0.0.1:19501", max_concurrency: 8}
  - {name: flaky, url: "http://127.0.0.1:19502", max_concurrency: 8}
  - {name: victim, url: "http://127.0.0.1:19503", max_concurrency: 8}
health: {interval: 1s}
cache: {enabled: true}
EOF
bin/siliconrouter -config "$WORK/router.yaml" > "$WORK/router.log" 2>&1 &
ROUTER=$!
for _ in $(seq 1 50); do curl -sf "localhost:$PORT/readyz" > /dev/null && break; sleep 0.2; done

metric() { curl -s "localhost:$PORT/metrics" | awk -v m="$1" '$1 == m { print $2 }'; }
sample() { echo "$(date +%s) $(metric siliconrouter_goroutines) $(metric siliconrouter_heap_bytes)" >> "$WORK/samples"; }

# Load: back-to-back benchmark rounds until time is up.
END=$(($(date +%s) + DURATION))
(
  while [ "$(date +%s)" -lt "$END" ]; do
    bin/srbench -url "http://127.0.0.1:$PORT/v1/chat/completions" -model llama \
      -conversations 24 -concurrency 12 -turns 4 -max-tokens 16 >> "$WORK/bench.log" 2>&1
  done
) &
LOAD=$!

T0=$(date +%s)
while [ "$(date +%s)" -lt "$END" ]; do
  sample
  ELAPSED=$(($(date +%s) - T0))
  if [ "$ELAPSED" -ge $((DURATION / 4)) ] && [ -n "${VICTIM:-}" ]; then
    echo "killing backend victim at ${ELAPSED}s"; kill "$VICTIM"; VICTIM=
    sleep 5; backend 19503 victim; echo "victim restarted"
    kill -HUP "$ROUTER"
  fi
  if [ "$ELAPSED" -ge $((DURATION / 2)) ] && [ -z "${RELOADED:-}" ]; then
    kill -HUP "$ROUTER"; RELOADED=1; echo "config reloaded at ${ELAPSED}s"
  fi
  sleep 5
done
wait "$LOAD"
sleep 3 # let in-flight requests finish
sample

fail=0
check() { if ! eval "$2"; then echo "FAIL: $1"; fail=1; else echo "ok:   $1"; fi; }
ok=$(awk '/^requests/ { s += $2 } END { print s + 0 }' "$WORK/bench.log")
bad=$(awk '/^requests/ { s += $4 } END { print s + 0 }' "$WORK/bench.log")
echo "requests: $ok ok, $bad failed (the flaky backend fails 10%; retries should hide nearly all)"
check "router still serving" 'curl -sf "localhost:$PORT/readyz" > /dev/null'
check "almost every request succeeded" '[ "$bad" -le $((ok / 100 + 5)) ]'
inflight=$(curl -s "localhost:$PORT/metrics" | awk '/^siliconrouter_backend_inflight/ { s += $2 } END { print s + 0 }')
check "no backend slots held after the load" '[ "$inflight" -eq 0 ]'
# Leak check: compare the second half of the run with the first (after
# warm-up). Steady state should stay within 50% plus a small margin.
read -r g1 h1 g2 h2 < <(awk '{ n++; g[n] = $2; h[n] = $3 } END {
  a = int(n / 4) + 1; b = int(n / 2); c = b + 1
  for (i = a; i <= b; i++) { ga += g[i]; ha += h[i] }
  for (i = c; i <= n; i++) { gb += g[i]; hb += h[i] }
  printf "%d %d %d %d\n", ga / (b - a + 1), ha / (b - a + 1), gb / (n - c + 1), hb / (n - c + 1) }' "$WORK/samples")
echo "goroutines: $g1 -> $g2, heap: $((h1 >> 20)) MiB -> $((h2 >> 20)) MiB"
check "goroutines steady" '[ "$g2" -le $((g1 * 3 / 2 + 20)) ]'
check "heap steady" '[ "$h2" -le $((h1 * 3 / 2 + 16 * 1024 * 1024)) ]'
check "no panics" '! grep -q "panic" "$WORK/router.log"'
[ "$fail" = 0 ] && echo "soak passed (${DURATION}s)" || { echo "soak FAILED; router log:"; tail -20 "$WORK/router.log"; }
exit "$fail"
