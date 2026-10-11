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

When admin keys are set, only they can use `/admin/*`: `GET /admin/backends`, `GET /admin/usage`
and `POST /admin/reload`. Client keys get `401` there, and admin keys can't be used for
inference. Without admin keys, any client key can use `/admin/*`, which is the old behavior. The router logs
a warning about this at startup; set admin keys whenever clients aren't fully trusted.

`/metrics`, `/healthz` and `/readyz` stay unauthenticated so Prometheus and load balancers can
reach them. They expose backend names and counts, not prompts or keys. Restrict them at the
network level if that matters.

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
| Model aliases, response cache settings | health-check and chipagent polling schedules |
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
- how many tokens each client used today, so `tokens_per_day` quotas hold across the fleet.

Nothing else is needed: no Redis, no database.

- **Consistency is eventual.** A turn that arrives within a second of the previous one, on another
  router, may miss the cache once. Quotas can overshoot by about one sync interval's worth of
  traffic.
- **`max_concurrent` and `requests_per_minute` apply per router.** For a fleet-wide rate, divide
  the limit by the number of routers.
- **All routers need the same backend names.** The response cache and the health view stay per
  router.

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
