# Running SiliconRouter in production

This page covers what a platform or security team checks before a router carries company
traffic: encryption, who can administer it, how each team's use is limited and measured, and how
to change it without downtime.

## HTTPS

```yaml
tls:
  cert_file: /etc/siliconrouter/tls.crt
  key_file: /etc/siliconrouter/tls.key
  client_ca_file: ""     # set to require client certificates (mutual TLS)
  min_version: "1.2"     # or "1.3"
```

- Served over HTTP/2 and HTTP/1.1. Plain HTTP to the port is refused.
- **Certificates rotate without a restart.** The files are checked every 30 seconds and on every
  reload, so cert-manager, Let's Encrypt (certbot) or a manual `cp` all just work. If a new file is
  broken, the router logs an error and keeps serving the old certificate.
- With `client_ca_file`, every client must present a certificate signed by one of those CAs, as
  well as an API key if keys are configured. The CA file is reloaded the same way.
- Backends can be `https://` URLs; the router verifies their certificates against the system
  trust store.

Without `tls`, the router speaks plain HTTP. That is fine behind a TLS-terminating load balancer
or service mesh, but not on an open network: prompts and API keys would cross it unencrypted.

## Admin access

```yaml
auth:
  admin_keys_env: SILICONROUTER_ADMIN_KEYS   # comma-separated; or admin_keys: [...]
```

When admin keys are set, only they can use `/admin/*`: `GET /admin/backends`, `GET /admin/usage`,
`POST /admin/reload` and the drain endpoints below. Client keys get `401` there, and admin keys can't be used for
inference. Without admin keys, any client key can use `/admin/*`, which is the old behavior. The router logs
a warning about this at startup; set admin keys whenever clients aren't fully trusted.

`/metrics`, `/healthz` and `/readyz` stay unauthenticated so Prometheus and load balancers can
reach them. They expose backend names and counts, not prompts or keys. Restrict them at the
network level if that matters.

## Maintenance: draining a backend

```bash
curl -X POST -H "Authorization: Bearer $ADMIN_KEY" https://router:8443/admin/backends/gpu-3/drain
# ... restart vLLM, swap the model, reboot the box ...
curl -X POST -H "Authorization: Bearer $ADMIN_KEY" https://router:8443/admin/backends/gpu-3/undrain
```

A drained backend gets no new requests. Requests already running on it, streams included, finish
normally. Queued requests that only it could serve fail at once with `503` rather than wait for it.
Both calls return the backend's status. Watch `inflight` reach `0` before you take the machine
down. A drain lasts until `undrain` or a router restart, and it survives config reloads. Drained
backends drop out of `/v1/models` and `/readyz`.

In cluster mode, drain on every router: each one routes on its own view.

## Per-team limits and usage

```yaml
auth:
  keys:
    - name: chat-app
      key_env: CHAT_APP_KEY
      priority: high
    - name: analytics
      key_env: ANALYTICS_KEY
      max_concurrent: 20           # in flight or queued at once
      requests_per_minute: 600     # sustained; bursts up to a minute's worth
      tokens_per_day: 5000000      # prompt + completion, resets at 00:00 UTC
    - name: nightly-batch
      key_env: BATCH_KEY
      priority: low
      max_concurrent: 8
```

A request over a limit gets `429` with `Retry-After` and an error code of `concurrency_limit`,
`rate_limited` or `quota_exceeded`. The request never reaches a GPU. Limits apply per key name,
so one team can't fill the queue or exhaust the fleet for everyone else.

### What a key may use

```yaml
auth:
  keys:
    - name: interns
      key_env: INTERN_KEY
      models: ["llama-3.1-8b", "qwen2.5-*"]   # exact names, or a prefix ending in *
      max_tokens: 1024                         # completion budget per request
```

- **`models`** refuses (`403 model_not_allowed`) any request that names a model outside the
  list. This includes models named in the URL (Gemini, Triton, Azure-style deployments) and in the
  query string. A request may name an alias whose target is allowed. A request that names no model
  is refused, and so are WebSocket and gRPC sessions that don't name one in the URL.
- **`max_tokens`** refuses (`400 max_tokens_exceeded`) a request whose completion budget is larger
  than the cap. The router checks every field an API might read: `max_tokens`,
  `max_completion_tokens`, `max_output_tokens`, TGI's `max_new_tokens`, Ollama's `num_predict`
  (where `-1` means unlimited, so it's refused), and Gemini's `maxOutputTokens`. When an OpenAI- or
  Anthropic-style request sets no budget, the cap is added to it. WebSocket and gRPC sessions are
  refused for capped keys, because the router doesn't read the messages inside them.

Keys with a policy get strict parsing. Some backends read JSON keys case-insensitively (Go's
`encoding/json`) or take the last of two duplicate keys, so a request that writes a policy field
twice, with escapes (`"m\u006fdel"`), or in another case (`"Model"`) is refused with `400`.
Otherwise the backend could read a different model or budget than the router checked. Keys
without a policy are forwarded byte-for-byte, as before.

**Usage** is counted per key name. Keys from `api_keys` count as `default`, and requests with auth
off count as `anonymous`.

- `GET /admin/usage`: requests, rejections, in-flight, prompt and completion tokens, tokens used
  today against the quota.
- `siliconrouter_client_requests_total{client}` and
  `siliconrouter_client_tokens_total{client,type,source}` for Prometheus, Grafana, and chargeback
  reports.

Token counts come from what the backend reports, in each API's own format: OpenAI-compatible
`usage` (vLLM, SGLang, llama.cpp, TGI's OpenAI API), Anthropic `usage`, Gemini `usageMetadata`,
and Ollama's `eval_count`. When a backend doesn't report usage, the router estimates it and labels
it `source="estimated"`:
- the prompt from its length;
- a stream's output from its number of events;
- a JSON response from its size.

For exact numbers on OpenAI-style streams, have clients send
`"stream_options": {"include_usage": true}`.

Counters live in memory, and a config reload keeps them. To keep them across restarts, including
today's quota, set `state_dir` (see below). Use the Prometheus counters for long-term reporting.

## Changing config without downtime

Edit the config file, then either:

```bash
kill -HUP $(pidof siliconrouter)
curl -X POST -H "Authorization: Bearer $ADMIN_KEY" https://router:8443/admin/reload
```

The new file is validated first. If anything is wrong, nothing changes, and the error is logged
(and returned by the API). Otherwise these take effect for new requests at once, while in-flight
requests and streams finish undisturbed:

| Takes effect immediately | Needs a restart |
|---|---|
| Backends: add, remove, change capacity, tier, models, URL or credentials | `listen` |
| API keys, admin keys, priorities, limits (usage counters are kept) | turning `tls` on or off (certificate changes are live) |
| Model aliases and fallbacks, response cache settings, `discovery` | health-check and chipagent polling schedules |
| Queue, retry and routing settings, `max_body_bytes` | `upstream` timeouts, prefix table size |

The response lists each change, and settings that need a restart are named as such:

```json
{"status": "applied", "changes": ["added backend gpu-5", "updated API keys, admin keys or limits",
 "listen changed: takes effect after a restart"]}
```

A backend whose URL or credentials change is replaced: new requests use the new endpoint and its
health is checked afresh, while requests already running on the old endpoint finish there. A
backend whose capacity, tier or models change keeps its health, load and hardware data.

## Several routers (cluster mode)

Run two or more routers behind your load balancer for redundancy:

```yaml
cluster:
  peers: ["https://router-0:8443", "https://router-1:8443", "https://router-2:8443"]
  secret_env: SILICONROUTER_CLUSTER_SECRET   # same value on every router, 16+ characters
  # node_name defaults to the hostname; sync_interval to 1s
```

Every router can use the same config, listing them all; a router skips itself. Each router sends
its peers two things every `sync_interval`:
- which backend now holds each conversation prefix, so a follow-up turn that lands on another
  router still reaches the GPU with its cache;
- each client's token use today and requests in flight, so `tokens_per_day` and `max_concurrent`
  hold across the fleet.

`requests_per_minute` is shared among the live routers by demand. Each router reports how often
each client has been sending it requests (a 30-second moving average, rejected attempts
included). It then admits:
- 20% of the limit split evenly between the live routers, so a router whose traffic just started
  can admit some requests at once;
- plus 80% in proportion to its share of that client's demand.

With a limit of 600 and three routers:
- traffic spread evenly gives each router about 200 a minute;
- if one router gets nearly all of the client's traffic (sticky load balancing by client IP, say),
  it admits about 520 and the other two about 40 each.

Either way the fleet admits about 600, and shares follow shifts in traffic within seconds. A
router counts as live while its pushes keep arriving, and stops counting three missed sync
intervals after it goes quiet. When a router dies, the others take over its share.
`GET /admin/usage` shows each client's `demand_per_minute` on this router and its current
`rate_share_per_minute`.

Nothing else is needed: no Redis, no database.

- **Consistency is eventual.** A turn that arrives within a second of the previous one, on another
  router, may miss the cache once. Quotas and concurrency limits can overshoot by about one sync
  interval's worth of traffic.
- **Bursts can briefly exceed the rate.** When traffic suddenly moves to another router, shares
  take a few seconds to follow, so the fleet may admit somewhat more, or less, than the limit for a
  moment.
- **All routers need the same backend names.** The response cache and the health view stay per
  router.

## Discovering backends from DNS

Instead of listing every GPU server, point the router at DNS:

```yaml
discovery:
  - name: vllm                          # backends are named vllm-<ip>:<port>
    dns: vllm-headless.ai.svc.cluster.local
    port: 8000
    agent_port: 9400                    # chipagent on each host (optional)
    interval: 15s
    backend: {tier: auto, max_concurrency: 32, health_path: /health}
  - name: tgi
    srv: _http._tcp.tgi.service.consul  # SRV records carry host and port
    backend: {tier: standard, models: [mistral-7b]}
```

Every A/AAAA record (with `dns`) or SRV target (with `srv`) becomes a backend with the settings
under `backend`, which takes the same fields as a `backends` entry except the name and URLs.
Lookups repeat every `interval`:
- new addresses join after their first health check;
- vanished addresses are removed, and their in-flight requests finish.

A failed lookup, or one that returns no records, keeps the last known set, so a DNS blip doesn't
empty the pool. Health checks still take out servers that are really gone.

For Kubernetes, use a headless Service (`clusterIP: None`) so DNS returns one record per ready
pod. A config can consist of `discovery` alone. Static `backends` and discovered ones mix freely.
Discovered backends survive config reloads, and changes to `discovery` itself apply within a
second. SRV priority and weight are ignored: capacity comes from `max_concurrency`.

## Keeping state across restarts

```yaml
state_dir: /var/lib/siliconrouter
```

The response cache and per-client usage counters are saved on shutdown and every 5 minutes, and
restored at startup. Each save writes a temporary file and renames it, so a crash never leaves a
half-written one. Today's token use counts toward the quota after a restart, and still resets at
midnight UTC.

## Tracing and logs

```yaml
tracing:
  otlp_endpoint: http://otel-collector:4318   # any OpenTelemetry collector, Jaeger, Tempo, ...
  sample_rate: 0.1                            # share of new traces exported
logging:
  request_sample_rate: 0.1                    # share of successful requests logged
  slow_request: 5s                            # always log requests slower than this
```

- Every request joins the caller's W3C `traceparent`, or starts a new trace, and passes it on to
  the backend. Engines that trace (vLLM, TGI, Triton) then show up as children of the router's
  spans.
- With an endpoint set, the router exports spans over OTLP/HTTP for the request, its time in the
  queue, and each backend attempt (with time to first token). It needs no OpenTelemetry SDK.
- Request log lines include `trace_id`. With sampling, errors and slow requests are always logged.

[`deploy/grafana/siliconrouter.json`](../deploy/grafana/siliconrouter.json) is a Grafana dashboard
over the router's metrics: traffic, latency, time to first token, cache results, backends,
hardware, and per-team usage and rejections.

## Deployment

| | |
|---|---|
| **Kubernetes** | [`deploy/helm/siliconrouter`](../deploy/helm/siliconrouter): routers as a StatefulSet (cluster mode is set up automatically when `replicaCount > 1`), optional TLS secret, persistent `state_dir`, PodDisruptionBudget, ServiceMonitor, and a chipagent DaemonSet for GPU nodes |
| **systemd** | [`deploy/systemd`](../deploy/systemd): hardened units; `systemctl reload siliconrouter` applies config changes live, and the config is validated before each start |
| **Containers** | `ghcr.io/<owner>/siliconrouter` and `ghcr.io/<owner>/chipagent`, multi-arch (amd64, arm64), published on each release |
| **Binaries** | GitHub releases: router for Linux, macOS and Windows (amd64, arm64); chipagent for Linux amd64/arm64, macOS and Windows; all with `SHA256SUMS`. Push a `v*` tag to publish one |

Validate a config file before deploying it:

```bash
siliconrouter -check-config -config siliconrouter.yaml
```
