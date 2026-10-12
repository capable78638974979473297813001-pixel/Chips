#!/usr/bin/env python3
"""Writes deploy/grafana/siliconrouter.json, the Grafana dashboard.

Edit the panels below and run `make dashboard`. Every query uses only
metrics the router exports (see the metric names in internal/server).
"""
import json
import pathlib

DS = {"type": "prometheus", "uid": "${datasource}"}
panels = []
y = 0
next_id = 1


def _id():
    global next_id
    next_id += 1
    return next_id - 1


def row(title):
    global y
    panels.append({"type": "row", "title": title, "collapsed": False, "id": _id(),
                   "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}})
    y += 1


def targets(queries):
    out = []
    for i, q in enumerate(queries):
        expr, legend = q if isinstance(q, tuple) else (q, None)
        t = {"datasource": DS, "refId": chr(ord("A") + i), "expr": expr}
        if legend:
            t["legendFormat"] = legend
        out.append(t)
    return out


def stat(x, title, query, unit="none", thresholds=None, desc=""):
    p = {"type": "stat", "title": title, "datasource": DS, "id": _id(), "description": desc,
         "gridPos": {"h": 4, "w": 4, "x": x, "y": y},
         "fieldConfig": {"defaults": {"unit": unit}, "overrides": []},
         "options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "value", "graphMode": "area"},
         "targets": targets([query])}
    if thresholds:
        p["fieldConfig"]["defaults"]["thresholds"] = {
            "mode": "absolute", "steps": [{"color": c, "value": v} for v, c in thresholds]}
    panels.append(p)


def series(x, w, title, queries, unit="short", stack=False, desc="", h=8, kind="timeseries", min0=True):
    p = {"type": kind, "title": title, "datasource": DS, "id": _id(), "description": desc,
         "gridPos": {"h": h, "w": w, "x": x, "y": y},
         "fieldConfig": {"defaults": {"unit": unit, "custom": {}}, "overrides": []},
         "targets": targets(queries)}
    if kind == "timeseries":
        if stack:
            p["fieldConfig"]["defaults"]["custom"]["stacking"] = {"mode": "normal"}
        if min0:
            p["fieldConfig"]["defaults"]["min"] = 0
    if kind == "bargauge":
        p["options"] = {"orientation": "horizontal", "displayMode": "gradient",
                        "reduceOptions": {"calcs": ["lastNotNull"]}}
        for t in p["targets"]:
            t["instant"] = True
    panels.append(p)


def advance(h):
    global y
    y += h


RED_IF_ANY = [(None, "green"), (1, "red")]

row("Overview")
stat(0, "Requests / s", "sum(rate(siliconrouter_requests_total[5m]))", "reqps")
stat(4, "p95 latency", "histogram_quantile(0.95, sum by (le) (rate(siliconrouter_request_duration_seconds_bucket[5m])))", "s")
stat(8, "Error ratio (5xx)",
     'sum(rate(siliconrouter_requests_total{code=~"5.."}[5m])) / clamp_min(sum(rate(siliconrouter_requests_total[5m])), 1e-9)',
     "percentunit", [(None, "green"), (0.01, "orange"), (0.05, "red")])
stat(12, "Backends in rotation", "sum(siliconrouter_backend_healthy) / count(siliconrouter_backend_healthy)", "percentunit",
     [(None, "red"), (0.5, "orange"), (0.99, "green")])
stat(16, "Queue depth", "sum(siliconrouter_queue_depth)", "none", [(None, "green"), (50, "orange"), (500, "red")])
stat(20, "Spend, last 24h", "sum(increase(siliconrouter_client_cost_dollars_total[24h])) + sum(increase(siliconrouter_backend_idle_cost_dollars_total[24h]))",
     "currencyUSD", desc="Request cost plus idle hardware cost, from cost_per_hour and token prices.")
advance(4)
series(0, 12, "Requests by backend and status", [("sum by (backend, code) (rate(siliconrouter_requests_total[5m]))", "{{backend}} {{code}}")], "reqps", stack=True)
series(12, 12, "Latency", [
    ("histogram_quantile(0.50, sum by (le) (rate(siliconrouter_request_duration_seconds_bucket[5m])))", "p50"),
    ("histogram_quantile(0.95, sum by (le) (rate(siliconrouter_request_duration_seconds_bucket[5m])))", "p95"),
    ("histogram_quantile(0.95, sum by (le) (rate(siliconrouter_queue_wait_seconds_bucket[5m])))", "queue wait p95"),
    ("histogram_quantile(0.95, sum by (le) (rate(siliconrouter_upstream_ttfb_seconds_bucket[5m])))", "upstream first byte p95"),
], "s")
advance(8)

row("Reliability")
stat(0, "Stalled engines", "sum(siliconrouter_backend_engine_stalled)", thresholds=RED_IF_ANY,
     desc="Engines with requests running but almost no tokens generated; they get no new work.")
stat(4, "Failed GPUs", "sum(siliconrouter_backend_hardware_faults)", thresholds=RED_IF_ANY,
     desc="Accelerators chipagent reports as failing (e.g. fallen off the bus).")
stat(8, "Retry budget exhausted / min", "sum(rate(siliconrouter_retry_budget_exhausted_total[5m])) * 60",
     thresholds=[(None, "green"), (0.1, "orange"), (5, "red")],
     desc="Failed attempts passed to clients without a retry because retries hit retry.budget_ratio.")
stat(12, "Router panics (1h)", "sum(increase(siliconrouter_panics_total[1h]))", thresholds=RED_IF_ANY)
stat(16, "Goroutines", "sum(siliconrouter_goroutines)", desc="Steady growth means a leak.")
stat(20, "Heap", "sum(siliconrouter_heap_bytes)", "bytes")
advance(4)
series(0, 8, "Backends out of rotation", [
    ("1 - siliconrouter_backend_healthy", "{{backend}} unhealthy"),
    ("siliconrouter_backend_engine_stalled", "{{backend}} stalled"),
    ("clamp_max(siliconrouter_backend_hardware_faults, 1)", "{{backend}} GPU fault"),
], desc="1 while a backend gets no new work, by cause.")
series(8, 8, "Retries, failovers and budget", [
    ("sum(rate(siliconrouter_retries_total[5m]))", "retries"),
    ("sum by (backend) (rate(siliconrouter_first_token_timeouts_total[5m]))", "first-token failover {{backend}}"),
    ("sum(rate(siliconrouter_retry_budget_exhausted_total[5m]))", "budget exhausted"),
], "ops")
series(16, 8, "Upstream errors", [("sum by (backend) (rate(siliconrouter_upstream_errors_total[5m]))", "{{backend}}")], "ops")
advance(8)

row("Engine load")
series(0, 8, "Requests queued inside engines", [("siliconrouter_backend_engine_waiting", "{{backend}}")],
       desc="From each engine's own metrics (metrics_path): includes traffic from other routers and direct clients.")
series(8, 8, "KV-cache usage", [("siliconrouter_backend_engine_kv_cache_usage", "{{backend}}")], "percentunit")
series(16, 8, "In flight (router) and saturated engines", [
    ("siliconrouter_backend_inflight", "{{backend}} in flight"),
    ("siliconrouter_backend_engine_saturated", "{{backend}} saturated"),
])
advance(8)

row("Speed")
series(0, 8, "Time to first token by backend", [("siliconrouter_backend_ttft_seconds", "{{backend}}")], "s")
series(8, 8, "Prefix-cache routing", [("sum by (result) (rate(siliconrouter_prefix_cache_routing_total[5m]))", "{{result}}")], "ops", stack=True)
series(16, 8, "Response cache", [("sum by (result) (rate(siliconrouter_response_cache_total[5m]))", "{{result}}")], "ops", stack=True)
advance(8)

row("Autoscaling and LoRA")
series(0, 8, "Demand by model", [
    ("sum by (model) (siliconrouter_model_inflight)", "{{model}} in flight"),
    ("sum by (model) (siliconrouter_model_queued)", "{{model}} queued"),
    ("sum by (model) (siliconrouter_model_held)", "{{model}} held (cold start)"),
])
series(8, 8, "Backends: desired vs ready", [
    ("max by (model) (siliconrouter_model_desired_backends)", "{{model}} desired"),
    ("max by (model) (siliconrouter_model_ready_backends)", "{{model}} ready"),
], desc="desired_backends is what KEDA or an HPA can scale on (docs/OPERATIONS.md).")
series(16, 8, "LoRA placement and cold starts", [
    ("sum by (result) (rate(siliconrouter_lora_routing_total[5m]))", "LoRA {{result}}"),
    ("sum by (model) (rate(siliconrouter_cold_starts_total[5m]))", "cold start {{model}}"),
], "ops")
advance(8)

row("Hardware")
series(0, 8, "Accelerator utilization", [("siliconrouter_backend_accelerator_utilization_percent", "{{backend}}")], "percent")
series(8, 8, "Accelerator memory used",
       [('siliconrouter_backend_accelerator_memory_bytes{state="used"} / ignoring(state) siliconrouter_backend_accelerator_memory_bytes{state="total"}', "{{backend}}")],
       "percentunit")
series(16, 8, "Accelerators by vendor", [("sum by (vendor, kind) (siliconrouter_backend_accelerators)", "{{vendor}} {{kind}}")], kind="bargauge")
advance(8)

row("Teams and cost")
series(0, 8, "Spend per team, last 7 days", [("sort_desc(sum by (client) (increase(siliconrouter_client_cost_dollars_total[7d])))", "{{client}}")],
       "currencyUSD", kind="bargauge", desc="Each request's share of its backend's cost_per_hour while it ran, plus token prices. /admin/costs has the same by day.")
series(8, 8, "Spend per hardware, last 7 days", [
    ("sum by (hardware) (increase(siliconrouter_client_cost_dollars_total[7d]))", "{{hardware}} used"),
    ("sum by (hardware) (increase(siliconrouter_backend_idle_cost_dollars_total[7d]))", "{{hardware}} idle"),
], "currencyUSD", kind="bargauge")
series(16, 8, "Spend rate per team ($/hour)", [("sum by (client) (rate(siliconrouter_client_cost_dollars_total[15m])) * 3600", "{{client}}")],
       "currencyUSD", stack=True)
advance(8)
series(0, 8, "Tokens / s by client", [("sum by (client, type) (rate(siliconrouter_client_tokens_total[5m]))", "{{client}} {{type}}")], stack=True)
series(8, 8, "Requests / s by client", [("sum by (client) (rate(siliconrouter_client_requests_total[5m]))", "{{client}}")], "reqps", stack=True)
series(16, 8, "Rejected requests by reason", [("sum by (reason) (rate(siliconrouter_rejected_total[5m]))", "{{reason}}")], "reqps", stack=True)
advance(8)

dashboard = {
    "title": "SiliconRouter",
    "uid": "siliconrouter",
    "description": "Traffic, reliability, engine load, autoscaling, hardware and cost for SiliconRouter.",
    "tags": ["siliconrouter", "llm"],
    "timezone": "browser",
    "schemaVersion": 39,
    "version": 2,
    "refresh": "30s",
    "time": {"from": "now-6h", "to": "now"},
    "templating": {"list": [{"name": "datasource", "type": "datasource", "query": "prometheus", "label": "Prometheus"}]},
    "panels": panels,
}
out = pathlib.Path(__file__).resolve().parent.parent / "deploy" / "grafana" / "siliconrouter.json"
out.write_text(json.dumps(dashboard, indent=2) + "\n")
print(f"wrote {out} ({len(panels)} panels)")
