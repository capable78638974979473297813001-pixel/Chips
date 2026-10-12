#!/bin/sh
# Checks the quickstart: waits for the router to have a backend, sends one
# chat request through it, then a short multi-turn load test.
set -eu
ROUTER=${ROUTER:-http://localhost:8080}
MODEL=${MODEL_NAME:-qwen2.5-1.5b}

printf 'waiting for a backend to be ready (vLLM can take a few minutes on first start)'
i=0
until curl -sf "$ROUTER/readyz" > /dev/null; do
  i=$((i + 1))
  [ $i -gt 120 ] && { echo; echo "router not ready after 10 minutes; see: docker compose logs"; exit 1; }
  printf .; sleep 5
done
echo " ready"

echo "--- one request (via an alias an OpenAI app would use)"
curl -sS -D - "$ROUTER/v1/chat/completions" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","max_tokens":40,"messages":[{"role":"user","content":"Say hello in five words."}]}' \
  | grep -iE '^(HTTP|x-siliconrouter)|"content"' || true

echo "--- backends"
curl -sS "$ROUTER/admin/backends" | (command -v jq > /dev/null && jq -c '.backends[] | {name, healthy, inflight, engine}' || cat)

echo "--- load test"
docker compose run --rm bench 2> /dev/null || echo "(skipped: run from deploy/quickstart with the GPU compose file)"

echo "--- spend so far"
curl -sS "$ROUTER/admin/costs?days=1" | (command -v jq > /dev/null && jq -c '{total_dollars, idle_dollars, clients: [.clients[] | {client, dollars}]}' || cat)
echo "Grafana: http://localhost:3000   Prometheus alerts: http://localhost:9090/alerts"
