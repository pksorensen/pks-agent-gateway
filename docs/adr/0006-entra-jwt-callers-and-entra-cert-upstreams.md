# ADR 0006: Entra workload identity on both sides — `entra-jwt` callers, `entra-cert` upstreams

- Status: **Accepted** 2026-10-16
- Date: 2026-10-16
- Extends: [ADR 0002](0002-credential-sources.md) (adds the `entra-cert`
  source, the certificate path that ADR deferred), [ADR 0003](0003-bundle-and-subscription.md)
  (keyless subscriptions, principal limits)
- Deploy runbook: [docs/deploy/arvo-coolify.md](../deploy/arvo-coolify.md)

## Context

The first production use of the subscription lane is a central AI gateway on
the arvo Coolify box. Its first caller is one site (`gjelvand-map`, the
vandvaerk site). It uses two APIs on one Azure Foundry resource in Sweden
Central, both under the scope `https://cognitiveservices.azure.com/.default`:

- **`speech`**: Danish fast transcription,
  `POST /speechtotext/transcriptions:transcribe` on
  `contextand-cs-foundry.cognitiveservices.azure.com`, with a colon in the path.
- **`decision`**: Decision-1, `POST /providers/microsoft/v1/systemone` on
  `contextand-cs-foundry.services.ai.azure.com`. It takes small JSON bodies
  (capped at 64 KB) and is counted per request.

The requirements:

- **No shared secrets anywhere.** That rules out a `gwk_` key on the site and a
  Foundry key or Entra client secret on the gateway, because each of those is a
  string that two parties hold.
- **APIM + managed-identity posture.** Callers prove who they are with a token,
  and the gateway proves who it is to Foundry with a token. Foundry's data plane
  accepts Entra/RBAC anyway.
- **Not internet-exposed.** The gateway is reachable only on the box's docker
  network.
- **Spend is bounded per caller.** Foundry bills speech per audio second.

The box is not in Azure, so there is no managed identity. The nearest equivalent
that involves no secret is a **certificate credential**: an asymmetric key that
never leaves the machine. The public half is registered on the app.

Two tenants are involved and must not be conflated:

- the **issuer tenant**, where the callers and the gateway's own API app
  registration live;
- the **upstream tenant**, which owns the Foundry resource (the Contextand
  tenant).

## Decision

### 1. Upstream: `source: entra-cert`

A credential with per-credential `tenant`, `clientId`, `scope`, `keyFile` and
`certFile`. There is no global `ENTRA_TENANT`; each credential names its own
upstream tenant.

- **Key handling.** Paths are confined to `GATEWAY_IDENTITY_DIR` (default
  `{USER_DATA_DIR}/identity`). Relative paths join it; absolute paths and symlinks
  must stay inside it. The key must be a regular file with mode `0600`, and the
  gateway refuses anything group- or world-readable. The key is read on every
  mint and never enters env, config, the store, a response or a log. Only the
  paths are stored.
- **Client assertion.** RS256 JWT. Header: `x5t` = base64url(SHA-1 of the cert
  DER), plus `x5t#S256`. Claims: `iss` = `sub` = clientId, `aud` = the exact
  token endpoint used (`{ENTRA_AUTHORITY_HOST}/{tenant}/oauth2/v2.0/token`), a
  random `jti`, and `iat`/`nbf` = now, `exp` = now + 10 min. It is posted as
  `client_credentials` with
  `client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer`.
- **Cache.** Tokens are cached per **(tenant, clientId, scope)** and refreshed
  5 min before expiry (or at half-life when the lifetime is shorter). A
  single-flight mint means a cold burst makes one token request. The mint runs
  detached from the first caller's cancellation. `cred rotate` and `cred delete`
  drop the cached token.
- **Who may bind a key.** Creating an `entra-cert` credential requires the realm
  `GatewayAdmin` role, not just owner-admin. The key files are host material
  that every owner on the gateway could otherwise point at.
- **Key generation.** `gateway identity init [--dir] [--bits 2048|3072]` writes
  `key.pem` (PKCS#8, `0600`, `O_EXCL`) and a self-signed `cert.pem` (2 years) to
  a `0700` directory. It prints the thumbprint (hex SHA-1, as Entra shows it),
  `x5t`, the expiry, and a ready `keyCredentials` entry. If the pair already
  exists it only describes it, so it is safe on every start. With half a pair it
  refuses and does not repair.

**Rotation.** An app may hold several `keyCredentials`.
`PATCH /applications/{id}` **replaces the whole list**, so always send old and
new together.

1. Run `gateway identity init --dir /data/identity/next`.
2. Read the app's current `keyCredentials`, then PATCH it with the existing
   entries plus the new one. Both certificates are now valid.
3. Move `next/key.pem` and `next/cert.pem` over the live pair (`mv` is an
   atomic rename on the same volume), then run `gateway-cli cred rotate <id>`
   so the cached token is dropped and the next mint uses the new key.
4. Verify a call, then PATCH the list again without the old entry.

### 2. Callers: `entra-jwt` on `/apis/`

Enabled by `ENTRA_JWT_TENANT` (a GUID) and `ENTRA_JWT_AUDIENCES` (a
comma-separated list).

The gateway accepts exactly one token version, **v2**. A v2 token has
`iss = {authority}/{tid}/v2.0`, `aud` = the gateway API app's client id (a
GUID), and the caller in `azp`. v1 tokens (`iss https://sts.windows.net/{tid}/`,
`aud api://…`, caller in `appid`) are refused. Accepting both versions "to be
safe" would double the validation surface for no caller we have.

Validation:

- `alg` is `RS256` only. `none` and HS256 are rejected before any key lookup,
  and `kid` is required.
- `iss` must match exactly, `tid` must equal the configured tenant, and `ver`
  must be `2.0`.
- `aud` must be in the allowed list.
- `exp` is required. `exp` and `nbf` are checked with a 2 min skew.
- The token must be **app-only**: no `scp`, and `idtyp` either absent or `app`.

JWKS comes from `{authority}/{tid}/v2.0/.well-known/openid-configuration`. The
discovered issuer must equal the configured one, and `jwks_uri` must be on the
authority host. Fetching is lazy: a boot without network still starts, and
requests fail closed until a fetch succeeds. Keys are refreshed after 24 h, or on
an unknown `kid` at most once every 30 s, so garbage tokens cannot turn the
gateway into a JWKS amplifier. A failed refresh keeps serving the known keys.

**Authorization.** `azp` maps to a **Principal** with a matching `appId`, unique
per owner and compared case-insensitively. That principal's active subscriptions
and their bundles decide which APIs it may call, which is the same model as
keys.

| Case | Status |
|---|---|
| Bad, expired, wrong-issuer, wrong-audience or delegated token | 401 |
| Valid token, no principal for its `appId` | 403 |
| Principal disabled, or no subscription covers the API | 403 |

The principal registry is the allowlist. An app in the issuer tenant that can
obtain a token for the gateway audience still gets nothing until an operator
registers its `appId`.

**Keyless subscriptions.** `{"keyless": true}` mints no `gwk_` keys and stores
empty hashes, which can never match. `regenerate` on such a subscription is a
409, and a keyless subscription requires a principal with an `appId`.

The caller's `Authorization` header is stripped before the upstream call, as a
key would be.

The MCP OAuth path (`MCP_OAUTH_ISSUER`, Keycloak, resource-URL audience) is
separate and unchanged.

### 3. Spend and abuse limits

- **Principal limits** (`limits` on create, or `PUT …/principals/{id}/limits`):
  - `requestsPerMinute`: a fixed per-minute window.
  - `audioSecondsPerDay`: per UTC day.

  Both are checked after authorization and **before** a token is minted
  upstream. A rejected request is a 429 with `Retry-After` and is not charged.
  An admitted request is charged even if the upstream then fails (for example a
  502), which errs on the conservative side.
- **Audio estimate.** `ceil(Content-Length / audioBytesPerSecond)`. The default is
  32000 B/s (16 kHz PCM16 mono WAV). The multipart envelope and the `definition`
  JSON over-count slightly, which is conservative. Compressed audio is
  under-counted; set `audioBytesPerSecond` on the operation for such inputs.
- **Restarts.** The daily total is seeded from that day's usage records, so a
  restart does not reset a quota.
- **Metered operations** (`"meter": "audio"`) require `Content-Length` and answer
  411 without it.
- **Body cap.** Per-operation `maxBodyBytes`: `0` = the gateway default, `-1` =
  no cap. The default is **6 MiB** (`GATEWAY_DEFAULT_MAX_BODY_BYTES`).
  - This is a behavior change: it applies to every existing operation that
    declares no cap, including `kind: llm` APIs.
  - A declared `Content-Length` over the cap is a 413 before proxying.
  - A chunked body is cut at the cap by `MaxBytesReader`, which also gives a 413.
  - Bodies still stream; nothing is buffered.
- **Usage records** gain `bytesIn` and `audioSeconds`, and the usage totals
  expose them.

### 4. Kill switches

| Env | Effect |
|---|---|
| `GATEWAY_MODE=prod` | Refuses to start without `OIDC_ISSUER`; otherwise the management plane runs in dev mode and admits everyone as admin. Any other value except `dev` is a startup error. |
| `GATEWAY_PASSTHROUGH=off` | The Anthropic catch-all answers a local 404. The sim gate stays in front, so `sim-` keys still get their local 403. The default stays `on` for existing deployments. |
| `GATEWAY_SIM_ENABLED=1` | Required for the simulator, as before. |
| `GATEWAY_REALTIME_SPEECH_ENABLED=1` | Now required in addition to `SPEECH_RUNTIME_URL` for `/v1/speech/realtime`. |

One startup line reports the mode, the management auth and the state of every
lane.

### 5. Colon paths

Operation paths may contain `:`. The lane rebuilds the upstream path from the
decoded value with `RawPath` cleared. Go's path encoding leaves `:` unescaped,
so both `transcriptions:transcribe` and `transcriptions%3Atranscribe` reach the
upstream as `/speechtotext/transcriptions:transcribe`, with the query and the
multipart body (boundary included) byte-identical. A test enforces this.

## Consequences

- No shared secret exists anywhere in the speech path:
  - The site holds its own private key for its own Entra client assertion.
  - The gateway holds its own private key.
  - Foundry trusts the gateway's service principal through RBAC.
- New operator steps, covered in the deploy runbook:
  - generate the identity and register it on the upstream app;
  - grant that service principal a Speech data-plane role on the Foundry
    resource;
  - set `requestedAccessTokenVersion = 2` on the gateway API app;
  - register each caller's `appId`.
- `entra-cert` makes the gateway's `/data` volume sensitive. Back it up as such,
  and never copy `identity/` off it.
- Limits are in-memory per process, seeded from disk. Running two replicas would
  each enforce the full limit. The deployment is single-replica.

## Not done (deliberately)

- **v1 tokens and multi-tenant issuers.** Not accepted. One issuer tenant per
  gateway.
- **App roles.** The `roles` claim is not checked; the `appId` → principal
  mapping is the allowlist. If wanted later, add a per-principal `requiredRole`.
- **MCP tool calls.** `callAPITool` is not audio-metered or rate-limited. Speech
  goes through `/apis/` only.
- **OTEL ingest.** `/otel/v1/*` remains unauthenticated. On a host-internal
  network that is acceptable; anywhere else, put it behind auth or turn it off.
- **Realtime speech.** The WS bridge's `SPEECH_GATEWAY_TOKEN` is still a shared
  bearer. It is off unless enabled, and should move to `entra-jwt` before it is
  used.
