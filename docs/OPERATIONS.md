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

Counters live in memory. A config reload keeps them, but a restart resets them, including today's
quota. Use the Prometheus counters for durable, long-term reporting.

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
