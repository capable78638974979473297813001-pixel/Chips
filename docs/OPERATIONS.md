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

`/metrics`, `/healthz`, `/readyz` and `/autoscale` stay unauthenticated so Prometheus, load
balancers and autoscalers can reach them. They expose backend and model names and counts, not
prompts or keys. Restrict them at the network level if that matters.

## Single sign-on (OIDC / JWT)

People can call the router with their company login instead of a shared API key. Any OpenID
Connect provider works (Okta, Microsoft Entra ID, Google, Keycloak, Auth0, Dex, Ping), and so
does anything that issues signed JWTs with a published JWKS.

```yaml
auth:
  oidc:
    issuer: https://login.example.com/realms/acme   # tokens must carry this iss
    audiences: [siliconrouter]     # ...and this aud (required, so tokens for other apps are refused)
    user_claim: email              # who the user is (default sub)
    groups_claim: groups           # their groups; dots reach nested claims (realm_access.roles)
    admin_groups: [platform-admins]  # may use /admin/*
    teams:                         # first group that matches wins
      - group: ml-research
        name: ml-research          # default: the group name
        priority: high
        tokens_per_day: 50000000
      - group: support-eng
        name: support
        models: ["llama-3.1-8b", "support-bot-*"]
        max_tokens: 2048
    default_team: oidc             # team of users in none of the groups...
    require_team: false            # ...or refuse them with 403 no_team
    user_limits:                   # every user, on their own, on top of the team's limits
      max_concurrent: 4
      requests_per_minute: 60
      tokens_per_day: 2000000
```

Clients send `Authorization: Bearer <access token>`, the same header OpenAI SDKs use for keys, so
an app passes the user's token where it used to pass a key. Tokens and API keys work side by side:
anything shaped like a JWT is checked as a token, and everything else as a key.

What's checked:
- **The signature**, against the issuer's keys from OIDC discovery (`jwks_url` or `jwks_file`
  skip discovery). Only RS/PS256-512, ES256-512 and EdDSA are accepted. `none`, HMAC algorithms
  and RSA keys under 2048 bits are refused.
- **`iss`, `aud`, `exp` and `nbf`**, with `leeway` (default 60s) for clock skew.

Keys are cached and re-fetched every `jwks_refresh` (default 1h), and also when a token names a
key ID the router hasn't seen, which is how providers rotate keys. Fetches triggered by tokens are
limited to one per 30 seconds, so forged tokens can't flood the provider. Verified tokens are
cached until they expire, so a request costs a hash lookup, not an RSA check. If the provider is
unreachable at startup, the router logs a warning and keeps trying.

**Teams and users.** A user's team works like a named API key: priority, limits, model allow-list
and token cap. Usage, cost and rate limits are counted under the team name, so chargeback reports
stay per team. Each user is also counted and limited on their own as `user:<id>`, so one person
can't use up their whole team's quota. `/admin/usage` lists both. Prometheus metrics are per team
only, to keep the number of series bounded. Requests from different users never share
response-cache entries.

Errors: `401 token_expired`, `401 invalid_token` (with the reason), `403 no_team`, and
`503 auth_unavailable` when the provider's keys can't be fetched at all.

## Audit log

```yaml
audit:
  file: /var/lib/siliconrouter/audit/audit.jsonl   # or "-" for stdout
  requests: true                   # one entry per inference request (default)
  max_size_mb: 100                 # rotate to audit.jsonl.1, .2, ...
  max_files: 10
```

The audit log is append-only JSON lines. It records:
- **request**: who (`client` team or key name, `actor` as `user:<id>` or a key fingerprint like
  `key:3fa2c1d0`), method, path, model, backend, status, attempts, tokens, cost, duration, remote
  address and trace ID. It never records prompt or response content.
- **auth_failure**: bad or expired keys and tokens, with the reason and the remote address.
- **admin**: every `/admin/*` call, including reads, with the actor and the result.
- **config_applied**: every reload, with what changed.
- **startup**.

Each entry carries a sequence number and the SHA-256 of the previous entry, and its own hash covers
both. Editing, deleting or reordering entries breaks the chain. The chain continues across
rotations and restarts. Check it with:

```bash
siliconrouter -verify-audit audit.jsonl.2 audit.jsonl.1 audit.jsonl   # oldest first
# audit log OK: 48211 entries, chain intact
```

The chain catches edits to some entries. It can't stop someone who can rewrite the whole file from
writing a whole new chain. Ship the files to write-once storage or your SIEM (Splunk, Elastic,
Datadog, or anything that tails JSON lines) for that. Write errors are logged and don't fail
requests. API keys appear only as fingerprints. Admin keys are fingerprinted the same way, so you
can tell which key did what without the log revealing any key.

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

## Cost in dollars

Token counts don't answer the question finance asks: what did each team cost? Give each backend a
price, and the router turns GPU time into dollars per team and per kind of hardware.

```yaml
backends:
  - name: h100-node-1
    url: http://10.0.0.11:8000
    cost_per_hour: 31.20          # e.g. an 8x H100 instance; or amortized hardware + power
    hardware_name: H100           # how it's grouped in reports (default: chipagent's GPU name)
  - name: hosted-fallback
    url: https://api.example.com
    cost_per_million_prompt_tokens: 0.60
    cost_per_million_completion_tokens: 2.40
```

**How time becomes dollars.** At each moment, a backend's hourly cost is split evenly between the
requests running on it. A request that ran alone for 2 seconds on a $36/hour server cost 2 cents. If
three other requests shared the server for that time, it cost half a cent. The router keeps a
running "cost clock" per backend, so this costs nothing per request no matter how many share a
server. Time with nothing running is booked as **idle**, so the bill for unused capacity is visible
too. Token prices, when set, are added on top. Retries are charged to the client too: a failed
attempt used the GPU as well.

When a backend has `metrics_path`, the count of running requests is the larger of the router's own
and what the engine reports. Requests from other routers (cluster mode) or from clients that call
the engine directly then take their share, and this router's teams aren't billed for them. Without
engine metrics in cluster mode, each router splits the cost among only its own requests, so the
fleet-wide sum overcounts. Turn on `metrics_path` for cost reports across several routers.

**Reports:**

```bash
curl -H "Authorization: Bearer $ADMIN_KEY" 'localhost:8080/admin/costs?days=7'
curl -H "Authorization: Bearer $ADMIN_KEY" 'localhost:8080/admin/costs?from=2026-10-01&to=2026-10-31'
```

```json
{"from": "2026-10-06", "to": "2026-10-12", "currency": "USD",
 "total_dollars": 1904.17, "idle_dollars": 377.40,
 "clients": [{"client": "team-x", "dollars": 412.08,
              "by_hardware": {"H100": 398.51, "MI300X": 13.57},
              "by_day": {"2026-10-06": 61.20, "2026-10-07": 58.94}}],
 "hardware": [{"hardware": "H100", "dollars": 1501.33, "idle_dollars": 290.10}]}
```

Days are UTC. The ledger keeps 400 days in memory, and `state_dir` keeps it across restarts. Each
request's cost is also in its log line (`cost_usd`). For Prometheus, use
`siliconrouter_client_cost_dollars_total{client,hardware}` and
`siliconrouter_backend_idle_cost_dollars_total{backend,hardware}`. For example,
`sum by (client) (increase(siliconrouter_client_cost_dollars_total[7d]))` gives spend per team per
week across every router. The Grafana dashboard has these panels.

## Scale-to-zero and autoscaling

GPUs are expensive to leave idle. The router helps you turn them off: it holds requests while a
server boots, wakes whatever starts servers, and publishes the demand an autoscaler needs.

```yaml
autoscaling:
  cold_start_wait: 5m              # hold requests up to this long while a backend starts (0 = off)
  scale_to_zero_models: [llama-3.1-70b]   # may have no backends at all; still listed in /v1/models
  webhook:                         # optional: called when requests start waiting
    url: https://ops.example.com/hooks/start-gpu
    secret_env: SCALE_WEBHOOK_SECRET      # sent as Authorization: Bearer
    interval: 30s                  # at most one call per model this often
  target_utilization: 0.8          # desired_backends aims for this share of max_concurrency
  backend_capacity: 16             # max_concurrency assumed while a model has no backends
```

**Cold starts.** When `cold_start_wait` is set and no backend for a request's model is up, the
request waits in the queue for up to `cold_start_wait` instead of failing with `503` at once. This
covers backends that are all down, booting, stalled or on a failed GPU, and models in
`scale_to_zero_models` that have no backends at all. The request is answered as soon as a backend
for the model passes its health checks, including one that DNS discovery or a config reload just
added. If none comes up in time, the client gets `503 cold_start_timeout` with `Retry-After`.
Drained backends don't count, because a drain is an operator's decision. A request with
`models.fallbacks` left doesn't wait: it goes to the fallback model at once, while the webhook
still asks for the big model to start.

**The webhook** gets `POST {"event": "scale_up", "model": "...", "held_requests": 3, "router":
"...", "time": "..."}`. Use it to start a stopped VM, call a cloud API, or scale a Deployment
from a small function. Adapter requests ask for their base model.

**Signals for autoscalers.** Per model, counting adapters toward their base model:

| Metric | Meaning |
|---|---|
| `siliconrouter_model_inflight` | requests running |
| `siliconrouter_model_queued` | requests waiting for a slot, including held ones |
| `siliconrouter_model_held` | requests waiting because no backend for the model is up |
| `siliconrouter_model_ready_backends` | backends in rotation serving it |
| `siliconrouter_model_desired_backends` | `ceil((inflight + queued) / (max_concurrency x target_utilization))` |
| `siliconrouter_cold_starts_total` | requests that arrived with no backend up |

The same numbers are served as JSON at `GET /autoscale` and `GET /autoscale/<model>`, without
auth, like `/metrics`. KEDA's `metrics-api` scaler can read `desired_backends` from there.
[deploy/kubernetes/keda-scaledobject.yaml](../deploy/kubernetes/keda-scaledobject.yaml) scales a
vLLM Deployment between 0 and 8 replicas this way, with a Prometheus variant for cluster mode,
where each router only sees its own requests. An HPA can use the same metrics through
prometheus-adapter.

For scale to zero on Kubernetes, find the vLLM pods with DNS `discovery` on a headless Service.
New pods join as their health checks pass, and held requests go to them right away.

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

## Reliability: what the router does when things break

| Failure | What happens | Settings |
|---|---|---|
| A backend refuses connections, resets, or answers 429/502/503/504 | The request is retried on another backend before the client sees anything | `retry.max_attempts` |
| A streaming backend accepts a request and goes silent | Moved to another backend after `first_token_timeout` | `retry.first_token_timeout` |
| A backend keeps failing | Passive health takes it out after `unhealthy_threshold` failures; active checks bring it back | `health.*` |
| An engine hangs with requests "running" (hung GPU, deadlocked engine) | Detected from its token counters: no new work until tokens flow again. Queued requests only it could serve fail at once | `engine_metrics.stall_after`, `stall_min_tokens_per_second` |
| A GPU falls off the bus (NVIDIA Xid 79) | chipagent reports it; the backend gets no new work until a healthy report | chipagent on the machine |
| A GPU wedges the driver, so hardware reads hang | chipagent answers with its last good readings, marked stale, instead of hanging | `chipagent --collect-timeout` |
| The whole fleet is overloaded and failing | Retries are capped at 20% of recent traffic, so failures don't multiply into more load | `retry.budget_ratio`, `budget_min_per_second` |
| Non-streaming requests to a hung engine | They wait as long as the client does, unless you set a cap | `upstream.response_header_timeout` |
| A bug in the router | The request gets a 500 and the stack is logged; its slots are released. A background task that panics is restarted | `siliconrouter_panics_total` |
| Rolling update or scale-down | `/readyz` fails for `drain_delay` while the router still serves, then in-flight requests get up to `timeout` | `shutdown.*` |

**How this is tested:**
- `TestChaos` sends 300 concurrent requests to backends that misbehave in every way above, with
  clients hanging up at random. It then checks that every backend slot, queue place and per-client
  count is returned and no goroutines leak. A deliberately leaked slot makes it fail.
- `make soak` runs steady load for `DURATION` seconds (default 120). During the run one backend
  fails 10% of its requests, another is killed and restarted, and the config is reloaded twice. It
  fails on client errors, slots still held, panics, or goroutines or heap that keep growing.
  `siliconrouter_goroutines` and `siliconrouter_heap_bytes` show the same thing in production.

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
