# Kubernetes Gateway API Inference Extension

The [Gateway API Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/) is the
standard way Kubernetes routes AI traffic. Envoy Gateway, Istio, kgateway, GKE Gateway and the
llm-d stack implement it. You declare an **InferencePool** of model server pods, and the gateway
asks the pool's **endpoint picker** which pod should get each request.

SiliconRouter can be that endpoint picker. The gateway keeps owning the traffic: TLS, the HTTP
route, and the connection to the pod. SiliconRouter makes the routing decision, the same one it
makes when it proxies requests itself:

- prefix-cache affinity, so a conversation stays on the pod holding its KV cache;
- LoRA adapter affinity and on-demand adapter loading;
- engine queue and KV-cache load, and stall and GPU-fault detection;
- hardware tiers and context-size fit;
- per-team and per-user auth, limits and model policy (API keys or single sign-on);
- model aliases (the request body is rewritten);
- cold starts (requests wait while the first pod of a scaled-to-zero model boots);
- usage, cost and audit, read from the response as it streams back.

## How it talks to the gateway

The gateway calls SiliconRouter over Envoy's external processing gRPC protocol (ext_proc), as the
[endpoint picker protocol](https://github.com/kubernetes-sigs/gateway-api-inference-extension/tree/main/docs/proposals/004-endpoint-picker-protocol)
specifies:

- Request bodies are read in `FULL_DUPLEX_STREAMED` mode, the mode the extension requires. The
  router holds its answer until the whole body has arrived, picks a pod, then sends the body back
  in chunks of at most 62 KB. `BUFFERED` and `NONE` work too when Envoy announces them (Envoy 1.33+
  sends its processing mode on each stream).
- The chosen pod goes in the `x-gateway-destination-endpoint` header, which overwrites any value
  the client sent, and in the `envoy.lb` dynamic metadata, as `ip:port`.
- A subset hint (`envoy.lb.subset_hint` / `x-gateway-destination-endpoint-subset`) limits the
  choice to those endpoints. If none of them is usable, the answer is `503`.
- When no pod can take the request, the gateway is told to answer it directly. The answer is `503`
  when nothing can serve it, `429` with `Retry-After` for rate limits, quotas and queue overflow,
  and `401`/`403` for auth and policy. Clients get the same JSON errors as from the proxy.
- gRPC health checking answers `liveness` (always serving), and `readiness` and the ext_proc
  service (serving while some backend is in rotation and the router isn't draining).

gRPC runs on Go's own HTTP/2 server, and the protobuf messages are encoded by hand, so this adds no
dependencies. The listener serves TLS with a self-signed certificate by default, which is what
gateways expect from an endpoint picker. They connect with TLS and don't verify the certificate.
Set `secure_serving: false` for plaintext HTTP/2, or give it a `cert_file` and `key_file`.

```yaml
gateway:
  listen: ":9002"
  secure_serving: true
  # cert_file: /etc/siliconrouter/epp/tls.crt
  # key_file: /etc/siliconrouter/epp/tls.key
```

The proxy on `listen` keeps working alongside, so the same routers can serve clients directly and
pick endpoints for a gateway.

## On Kubernetes, with the Helm chart

```bash
helm install sr deploy/helm/siliconrouter \
  --set existingSecret=siliconrouter \
  --set gateway.enabled=true \
  --set gateway.inferencePool.create=true \
  --set gateway.inferencePool.selector.app=vllm-llama3-8b \
  --set gateway.inferencePool.targetPort=8000
```

This creates:

- an `InferencePool` (`inference.networking.k8s.io/v1`) that selects the vLLM pods and names the
  routers' Service, port 9002, as its `endpointPickerRef`;
- a headless Service over the same pods. The routers find every pod through it with DNS
  discovery, and health checks, engine metrics and LoRA state work per pod as usual.

Then send traffic to the pool from an HTTPRoute on your inference gateway:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: llm}
spec:
  parentRefs: [{name: inference-gateway}]
  rules:
    - backendRefs:
        - {group: inference.networking.k8s.io, kind: InferencePool, name: sr-pool}
```

With more than one router replica, the gateway spreads its ext_proc streams across them. Cluster
mode, which the chart turns on, shares prefix placement and limits between them, so either one
sends a conversation's next turn to the right pod.

## Without Kubernetes

[deploy/gateway/envoy-ext-proc.yaml](../deploy/gateway/envoy-ext-proc.yaml) is a plain Envoy
config that does what Envoy-based gateways generate. It has an ext_proc filter in
`FULL_DUPLEX_STREAMED` mode and an `ORIGINAL_DST` cluster that sends each request to the address
in `x-gateway-destination-endpoint`. It was tested with Envoy 1.35.3 in front of two `mockllm`
servers: multi-turn conversations stayed on one pod, model aliases were rewritten, streaming and
300 KB request bodies passed through, and auth and rate-limit rejections came back as `401` and
`429` with `Retry-After`.

## What's different from proxy mode

The gateway, not the router, connects to the pod. So in gateway mode the router can't:

- **retry on another backend** or fall back to another model after a failure. Configure retries on
  the gateway's route instead. The router's choice is a single endpoint.
- **answer from the response cache**.
- **detect a stalled first token**. Stall detection from engine metrics still works.

**Slot accounting.** The router holds a request's slot until the response ends. That's why gateways
should send response headers and bodies to the picker, as the extension's standard setup does.
If response processing is turned off, the slot is released when the gateway closes the stream.
That's usually right after the request is sent, so the router undercounts the load on each pod.
With `metrics_path` on the backends, the engines' own counts make up for most of it.

Logs and the audit log mark these requests with `kind=gateway`.
