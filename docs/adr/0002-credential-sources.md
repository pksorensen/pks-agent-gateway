# ADR 0002: Credential sources and injection

- Status: **Accepted** 2026-09-23 (vault: option A)
- Date: 2026-09-23
- Context doc: [PRD 0001](../prd/0001-api-gateway.md) (F13, F14, FR-13..15)

## Context

The subscription lane swaps a subscriber's key for the real upstream credential.
Poul's requirement is that several ways of holding that credential must work:
environment variables, secrets sealed inside the gateway, and, as the preferred
way, **pks-agent-vault**. Separately, Microsoft Foundry's data plane accepts
**Entra ID / RBAC only**, so for our first target the "credential" is a token
minted on demand, not a stored string.

APIM's equivalents are named values (plain, secret, Key Vault reference) and
backend auth modes (API key, OAuth2 client credentials, managed identity).

## Decision

A **Credential** is a management object with an id, a `source`, and an `inject`
spec. It resolves through one interface:

```go
type CredentialSource interface {
    // Resolve returns the value to inject and when to resolve it again.
    Resolve(ctx context.Context) (value string, refreshAt time.Time, err error)
}
```

| `source` | Configuration | Resolution | Rotation |
|---|---|---|---|
| `env` | `var: FOUNDRY_KEY` | `os.Getenv` at boot. The value is **not** stored in the folder store | restart or redeploy |
| `sealed` | value supplied once through the API or CLI (stdin, never argv) | AES-256-GCM at rest under `GATEWAY_SEAL_KEY` (env, 32 bytes); decrypted into memory | `gateway-cli cred rotate` replaces it with no restart |
| `vault` | `item: <owner>/<item>`, the gateway enrolled as a vault **agent keyholder** | released through the vault agent flow (two-share, single-use `releaseId`, PoP-signed) and held in memory until `refreshAt` | a new vault version takes effect on the next refresh; revoking the grant fails closed |
| `entra` | `tenant`, `clientId`, and a client secret or certificate (itself a `sealed`/`vault`/`env` credential), plus `scope` (e.g. `https://cognitiveservices.azure.com/.default`) | client-credentials token, cached until 5 min before `exp` | follows its inner credential |

**Injection** (`inject`): `{header: "api-key"}`, `{header: "x-api-key"}`,
`{bearer: true}` (`Authorization: Bearer …`), or `{query: "key"}`. The presented
subscription key is always stripped first.

Rules:

1. The management API and CLI **never return** a credential's value; they return
   only metadata: source, the last time it resolved, the next refresh.
2. A resolution failure makes the request fail with 502 and a generic body. The
   detail goes to the structured log **without the value**.
3. `entra` reuses the token-minting approach of pks-agent-azure's identity plane
   (the certificate-backed client-credentials flow). The first implementation may
   call a pks-agent-azure instance instead of minting in-process; which one is
   decided at build time.
4. `sealed` is the default when nothing else is configured. `vault` is the
   recommended source in docs and CLI help.

## Vault for a long-running server (decided: A)

A vault agent release is single-use, and pks-agent-vault requires a consent
answer for releases that live longer than an hour. Options:

- **A.** Re-release on a TTL shorter than one hour (≤ 55 min) with the consent
  mode that auto-approves standing grants. More vault traffic, but a revocation
  bites within the hour.
- **B.** Release once at boot or on rotation and hold the value in memory; this
  needs a consent answer each time. Cheaper, but a revocation bites only when the
  gateway restarts.

The recommendation is **A**. It matches vault's own posture ("revoking destroys
the server's half").

## Consequences

- `GATEWAY_SEAL_KEY` becomes required whenever a `sealed` credential exists; the
  gateway fails closed at boot if one exists and the key is missing.
- The gateway becomes a vault keyholder with its own enrollment. This is a new
  deployment step, and gets a runbook in the README.
- Every source must pass the canary-secret test from ADR 0001.
