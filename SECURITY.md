# Security policy

## Reporting a vulnerability

Please **don't open a public issue** for a security problem. Report it privately through GitHub:
**Security → Report a vulnerability** on this repository (private vulnerability reporting). Include
the version (`siliconrouter -version`), the relevant config with secrets removed, and steps to
reproduce.

What to expect:

| | |
|---|---|
| Acknowledgement | within 3 business days |
| First assessment (severity, affected versions) | within 10 business days |
| Fix for critical and high severity | target 30 days, sooner when exploited in the wild |
| Disclosure | coordinated with you; a GitHub Security Advisory and a CVE when warranted |

We credit reporters in the advisory unless you ask us not to.

## Supported versions

Until 1.0, only the latest minor release gets security fixes. Upgrade within it by changing the
patch version.

| Version | Supported |
|---|---|
| 0.4.x | yes |
| < 0.4 | never released |

## Scope

In scope: the router (`cmd/siliconrouter`, `internal/...`), `chipagent` (`agent/`), the Helm
chart, the Dockerfiles and the release pipeline. Examples of what we treat as vulnerabilities:

- getting past authentication or admin authorization: API keys, OIDC/JWT validation, admin keys or
  groups;
- one tenant reading another tenant's data, for example through the response cache;
- getting around per-key, per-team or per-user limits and model policy;
- breaking the audit log's hash chain without it being detected, or getting content (prompts,
  responses, keys) into the audit log;
- request smuggling, or header injection toward backends or the gateway, such as setting
  `x-gateway-destination-endpoint` yourself;
- crashing or hanging the router with crafted requests, beyond what the configured limits allow;
- vulnerabilities in the published binaries or images.

Out of scope: weaknesses of the inference engines behind the router (report those to vLLM,
SGLang, ...); deployments that ignore the warnings in [docs/OPERATIONS.md](docs/OPERATIONS.md),
such as no auth on an open network, or every client key allowed on `/admin/*`; and the
unauthenticated `/metrics`, `/healthz`, `/readyz` and `/autoscale` endpoints, which expose counts
and names by design.

## Hardening notes

- Turn on auth: named keys or `auth.oidc`. Set admin keys or `admin_groups`, otherwise every client
  can use `/admin/*`, and the router warns about this at startup.
- Serve TLS (`tls`), or terminate it in front of the router. API keys and prompts must not cross
  networks in the clear.
- OIDC: always set `audiences`. The router refuses to start without them, so a token issued to
  another application can't be replayed here.
- Restrict `/metrics`, `/autoscale` and the gateway (ext_proc) port to your network. The gateway
  port trusts its caller to be the gateway.
- Keep `cache.share_across_keys` off unless every client may see every other client's answers.
- Ship the audit log to write-once storage. The hash chain detects edits to some entries, but it
  can't stop someone who can rewrite the whole file.

## Verifying a release

Releases are built by [.github/workflows/release.yml](.github/workflows/release.yml) and signed
without long-lived keys, through [Sigstore](https://www.sigstore.dev/) and the workflow's GitHub
OIDC identity. Each release has:

- archives of the binaries, and `SHA256SUMS` covering them and the SBOM;
- `SHA256SUMS.sigstore.json`, a cosign signature bundle for `SHA256SUMS`;
- GitHub build provenance attestations for every archive;
- `siliconrouter-<version>.spdx.json`, an SPDX SBOM of the source and its Go dependencies;
- container images `ghcr.io/capable78638974979473297813001-pixel/siliconrouter` and `.../chipagent`,
  signed with cosign, each with an SBOM and SLSA provenance attestation attached.

Check a downloaded archive (here v0.4.0 on Linux):

```bash
V=v0.4.0
REPO=capable78638974979473297813001-pixel/Chips
ID="https://github.com/$REPO/.github/workflows/release.yml@refs/tags/$V"

# 1. The checksums file was signed by this repository's release workflow, for this tag.
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "$ID" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# 2. The archive matches the signed checksums.
sha256sum --check --ignore-missing SHA256SUMS

# 3. Optional: GitHub's provenance attestation for the archive.
gh attestation verify siliconrouter-$V-linux-amd64.tar.gz --repo "$REPO"
```

Check an image:

```bash
cosign verify ghcr.io/capable78638974979473297813001-pixel/siliconrouter:$V \
  --certificate-identity "$ID" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# Its SBOM and build provenance:
docker buildx imagetools inspect ghcr.io/capable78638974979473297813001-pixel/siliconrouter:$V \
  --format '{{ json .SBOM }}'
docker buildx imagetools inspect ghcr.io/capable78638974979473297813001-pixel/siliconrouter:$V \
  --format '{{ json .Provenance }}'
```

In Kubernetes, a policy controller such as Kyverno or the Sigstore policy-controller can require
these signatures before pods run. Use the same identity and issuer.

## Dependencies

The router depends only on the Go standard library and `gopkg.in/yaml.v3`. chipagent depends only
on the C++ standard library and POSIX. That keeps the supply chain small enough to read. The SBOM
lists everything that goes into each release, so scanners such as Grype and Trivy can check it.
