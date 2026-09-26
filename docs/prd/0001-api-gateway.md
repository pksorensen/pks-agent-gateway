# PRD 0001: pks-agent-gateway as an API gateway (APIM-style, agent-first)

| | |
|---|---|
| Status | **Accepted 2026-09-23** (function map + names); the remaining open items are in §9 |
| Date | 2026-09-23 |
| Owner | Poul |
| Supersedes | nothing. It extends the v1 passthrough proxy and the LLM simulator |
| Related | ADR 0001 three lanes · ADR 0002 credential sources · ADR 0003 bundle and subscription |

> Names (decided 2026-09-23): **Bundle** stands for APIM's *Product*;
> **Subscription** and **Principal** keep their names. The product-cli graph uses
> the same labels.

## 1. Problem

We give people and agents access to paid or protected upstream APIs (Foundry
transcription, Anthropic, internal HTTP services) by handing them the real
credential. That goes wrong in four ways:

- **Leakage.** The real key sits in every developer's `.env`, in agent session
  transcripts and in CI secrets. See `docs/secrets-and-coding-agents.md` in the
  parent repo: no coding agent redacts its own session files.
- **No per-person revocation.** Rotating the shared key breaks everyone.
- **No attribution.** Usage cannot be traced to a person. Today's gateway OTEL
  attribution is self-declared (`OTEL_RESOURCE_ATTRIBUTES=user=…`).
- **Agents can't discover what they are allowed to use.** There is no catalog
  and no ready-made MCP config.

Azure API Management answers this with a model we want to keep: APIs grouped into
products, and people subscribed to products, each with their own key. The gateway
checks that key and injects the real backend credential. Its new AI Gateway tier
adds models and **MCP tool servers** behind the same key. What we don't want from
it: Azure lock-in, a portal-and-XML operating model, and a developer-portal UI.

## 2. Product statement

**pks-agent-gateway is a self-hosted gateway where an operator publishes upstream
APIs and exposes them as HTTP routes and as MCP tools, bundles them, and gives each
person or agent a subscription with their own key. On every call the gateway checks
that key, applies policy, swaps in the real upstream credential, and records usage
against the subscription.**

It is **CLI- and agent-first**. It is configured only through a management API,
the `gateway-cli`, and a management MCP server. **There is no setup UI.**

It is not an identity provider (Keycloak and Entra are), not a secrets vault
(pks-agent-vault is), and not a billing system.

## 3. Personas

| Persona | Who | What they need |
|---|---|---|
| **Operator** | Poul, or an agent holding `GatewayAdmin` | Register APIs and credentials, make bundles, subscribe people, set limits, read usage — all from a terminal or an agent |
| **Human subscriber** | A developer or a customer contact | One key and one base URL or MCP config that "just works", and a way to see and rotate their own key |
| **Agent subscriber** | An ALP runner, Claude Code, a CI job, a pks-agent-* service | A non-human principal with its own subscription and a machine-readable `env` or MCP config, revocable without touching anything else |

## 4. APIM function map: the decision table

Legend: **Keep** means we build it. **Adapt** means we build our own variant.
**Later** means not in v1 but the model leaves room for it. **Drop** means no.

| # | APIM concept | Decision | Our name (proposed) | Notes |
|---|---|---|---|---|
| F1 | Service instance / Workspaces | Adapt | **Owner** | Multi-tenant (ADR 0005): owners are claimed via `/api/v1/owners` and appear in every URL; `owners/{owner}/…` on disk. A workspace is an owner. |
| F2 | API (REST, imported from OpenAPI) | Keep | **API** | Upstream base URL, public path prefix, an optional OpenAPI document. |
| F3 | Operations | Keep | **Operation** | Parsed from OpenAPI or declared by hand. The unit an MCP tool maps to. |
| F4 | Backend (+ pools, circuit breaker) | Adapt | **Upstream** | Folded into the API as `upstream`. Pools and failover are Later. |
| F5 | Models (AI Gateway tier: route by `model`, OpenAI-compatible + Anthropic passthrough) | Keep | **Model route** | An API of kind `llm`. Today's Anthropic proxy becomes one. Routing by exact `model` name is Later. |
| F6 | MCP server from a REST API (operations → tools) | **Keep, core** | **MCP server / Tool** | Tools only, like APIM: no MCP resources or prompts in v1. |
| F7 | MCP server passthrough to an existing server | Keep | **MCP server (remote)** | Streamable HTTP only. The deprecated SSE transport is Drop. |
| F8 | Product | Keep, **rename** | **Bundle** | "Product" collides with the agentics.dk products page and with product-cli. |
| F9 | Product "requires approval" | Later | — | Through pks-agent-consent (approval card on the phone). |
| F10 | Subscription (a named container for 2 keys; states active / suspended / cancelled / expired) | Keep | **Subscription** | Two live keys so rotation is not a flag day (the pks-agent-azure `cra_` pattern). |
| F11 | Subscription scopes: product / API / all-APIs | Adapt | — | v1 supports **bundle** scope only. All-APIs means an admin key and is Drop for subscribers. |
| F12 | Users / Groups | Adapt | **Principal** | Human = OIDC `sub` (Keycloak). Agent = service principal. Groups are Later. |
| F13 | Named values: plain / secret / Key Vault reference | Keep | **Credential** | Sources: `env`, `sealed`, `vault`, `entra` (ADR 0002). |
| F14 | Backend auth: none / API key / OAuth2 client credentials / managed identity | Keep | Credential **injection** | Header, query or bearer. `entra` mints tokens (Foundry is Entra/RBAC-only on its data plane). |
| F15 | Policies (XML + C# expressions, scopes global → product → API → operation) | Adapt | **Policy** | Typed, declarative, **no expression language**. v1 kinds: set/strip header, request rate limit, IP allow-list. |
| F16 | `llm-token-limit` (TPM, quotas per key) | Keep | Policy `token-limit` | Per subscription. Reads `usage` from the upstream response. |
| F17 | `llm-emit-token-metric` / logging | Keep | **Usage** | Attributed by the authenticated subscription, not self-declared. Existing `stats` is extended. |
| F18 | Semantic cache | Drop (v1) | — | Needs embeddings plus a vector store. Revisit if cost demands it. |
| F19 | Content safety | Drop (v1) | — | Out of scope for a gateway we operate ourselves. |
| F20 | Developer portal / self-service catalog | **Drop UI**, Adapt | **Catalog** | A read model over the management API, `gateway-cli catalog` and the management MCP server. |
| F21 | API inspector / request tracing | Later | — | The sim's cassette recorder is the seed. Must never write credential headers. |
| F22 | Revisions / versions | Later | — | Version APIs by giving them a new API id. |
| F23 | Management REST API / ARM / CLI / Bicep | Keep | **Management API** | `/api/v1/…` + `gateway-cli` + a management MCP server. Declarative `gateway-cli apply -f` is Later. |
| F24 | Self-hosted gateway | n/a | — | We *are* self-hosted: one Go binary, Coolify or ACA. |
| F25 | *(ours)* LLM simulator / testbench | Keep | **Testbench** | APIM has no equivalent. `sim-` keys still never reach an upstream. |

## 5. User stories

1. **Foundry transcribe, end to end.** As the operator, I register the Foundry
   transcription endpoint as an API with an `entra` credential. I bundle it as
   `speech` and subscribe Kim (a developer). Kim calls
   `https://gw…/apis/speech/…` with their own key. The gateway mints an Entra
   token, calls Foundry, streams the result back, and records usage against Kim's
   subscription. Kim never sees a Foundry credential.
2. **The same API as an MCP tool.** As the operator, I expose the transcribe
   operation as an MCP tool on server `speech-mcp`. Kim's agent lists and calls it
   at `https://gw…/mcp/speech-mcp` with the same key.
3. **The agent gets its config.** As an agent subscriber I run
   `gateway-cli sub env <subscription>`. I get the base URL, the key header, and a
   ready-to-merge `.mcp.json` fragment. Nothing else needs configuring.
4. **Revoke one person.** As the operator, I suspend Kim's subscription. Their next
   call gets 401 or 403 and nobody else notices.
5. **Rotate without downtime.** As a subscriber, I regenerate my secondary key,
   switch to it, then regenerate the primary.
6. **Credential from the vault.** As the operator, I attach a credential of source
   `vault` so the gateway resolves the upstream key from pks-agent-vault, and the
   plaintext never passes through my terminal.
7. **Limit spend.** As the operator, I put a `token-limit` of 200k tokens a day on
   the `llm` bundle. A subscriber over the limit gets 429 with `retry-after`.
8. **Agent operates the gateway.** As an agent holding `GatewayAdmin`, I use the
   management MCP server to register an API and subscribe a new runner, with no
   human at a UI.
9. **Existing users are unaffected.** Claude Code pointed at the gateway with its
   own `sk-ant-…` key keeps working byte for byte (passthrough lane), and
   `sim-scenario:*` keeps working (sim lane).

## 6. Functional requirements

**APIs and operations (F2–F5)**
- FR-1 Register, update, list and remove an API: id, display name, `kind`
  (`http` | `llm`), `upstream` URL, public path prefix, credential ref,
  subscription-required flag (default true).
- FR-2 Import operations from an OpenAPI 3.x document (JSON or YAML, by upload or
  URL). Declaring an operation by hand (method, path template, description, input
  schema) is also allowed.
- FR-3 Only paths matching a declared operation are forwarded. Anything else
  under the API prefix gets 404 **locally** and never reaches the upstream or the
  catch-all.

**MCP (F6–F7)**
- FR-4 Create an MCP server from an API by picking operations. Each operation
  becomes a tool whose name, description and `inputSchema` come from the OpenAPI
  operation. They can be overridden per tool.
- FR-5 Create an MCP server that proxies a remote Streamable-HTTP MCP server. The
  optional tool allow-list is enforced on `tools/list` and on `tools/call`.
- FR-6 MCP endpoint `/mcp/{owner}/{server}` (ADR 0005). The subscription key is accepted in the same
  headers as HTTP APIs. `tools/list` returns only the tools in bundles the
  subscription covers.
- FR-6a **MCP OAuth for clients that can't send a static header** (claude.ai
  connectors). The gateway is an OAuth **protected resource, never an
  authorization server**:
  - It serves protected-resource metadata (RFC 9728) under
    `/.well-known/oauth-protected-resource/mcp/{owner}/{server}`, naming the configured
    authorization server: Keycloak or Entra ID, one per owner.
  - A 401 carries `WWW-Authenticate` with `resource_metadata`.
  - The gateway validates the bearer JWT: issuer, signature, `aud` / resource =
    the MCP server URL, expiry.
  - It maps `sub` to a **Principal**, then to that principal's `active`
    Subscription on a bundle containing the server. The subscription gates and
    attributes the call exactly as a key would.
  - Known trap: Keycloak drops the RFC 8707 `resource` parameter, so the
    audience needs a mapper there.

**Bundles and subscriptions (F8–F12)**
- FR-7 Create a bundle; add or remove APIs and MCP servers; state `published` |
  `hidden`. Hiding a bundle does not invalidate existing keys (same as APIM).
- FR-8 Subscribe a principal (human `sub` or service principal) to a bundle. The
  response returns the primary and secondary key **exactly once**. The server
  stores only their hashes.
- FR-9 Subscription lifecycle: `active` → `suspended` ⇄ `active` → `cancelled`
  (terminal). Optional `expiresAt`, after which the state is `expired`.
- FR-10 Regenerate the primary or secondary key on its own. The operator can do
  it; so can the subscriber, for their own subscription.
- FR-11 Keys are presented in `api-key`, `Ocp-Apim-Subscription-Key`,
  `x-api-key` (for Anthropic SDK clients), or `Authorization: Bearer`. The
  gateway **strips** the presented key before forwarding.
- FR-12 Service principals: create, list, disable. A service principal
  authenticates to the data plane only through a subscription key.

**Credentials (F13–F14)**
- FR-13 A credential has an id, a `source` (`env` | `sealed` | `vault` | `entra`)
  and an `inject` spec (header name, query parameter, or bearer). The management
  API never returns a credential's value.
- FR-14 `sealed` values are encrypted at rest (AES-GCM under a gateway key from
  env). `env` values are resolved from a named env var. `vault` values are
  resolved through a pks-agent-vault agent grant. `entra` mints an access token
  for a resource or scope and caches it until shortly before it expires.
- FR-15 Credential rotation (`sealed` value replaced, vault version bumped) takes
  effect without a restart.

**Policies and usage (F15–F17)**
- FR-16 Policy kinds in v1: `set-header`, `strip-header`, `rate-limit`
  (requests per window), `token-limit` (tokens per window or quota period, `llm`
  APIs only), `ip-allow`. Scopes: global, bundle, API, subscription. The
  evaluation order is fixed: global → bundle → API → subscription.
- FR-17 Every data-plane call writes one usage record: subscription, principal,
  bundle, API or tool, status, latency, bytes. For `llm` APIs it also writes
  input and output tokens and the model.
- FR-18 Usage can be queried by subscription, principal, bundle or API and by
  day. The existing `/api/projects/{id}/stats` shape is kept for the old
  self-declared OTEL path.

**Owners (F1)**
- FR-0 Every management object lives under exactly one owner
  (`owners/{owner}/…`), and a subscription key only resolves APIs and MCP
  servers of its own owner. One deployment can serve several owners.

**Catalog and management (F20, F23)**
- FR-19 Catalog read: the bundles, APIs and tools a caller can see. A subscriber
  sees their own subscriptions (keys masked) and connection snippets.
- FR-20 Management API under `/api/v1/…`, OIDC bearer, roles `GatewayAdmin`
  (everything) and a subscriber's self-service on their own subscriptions.
- FR-21 `gateway-cli` covers every management verb:
  `api | op | mcp | bundle | sub | principal | cred | policy | usage | catalog`,
  plus `sub env`.
- FR-22 A management MCP server exposes the same verbs as tools for agents.
  Ownership is checked in every tool (`policy: 'read'` is not enough — a known
  house trap).

## 7. Non-functional requirements

- NFR-1 **Upstream secrets never leave the gateway.** They are not in responses,
  logs, OTEL, cassettes or error messages. Tested with a canary secret.
- NFR-2 **Lane isolation.** `sim-` keys never reach an upstream; the existing
  test `TestGateNeverProxiesSimKeys` stays. Passthrough traffic is never given an
  injected credential. Subscription traffic never reaches the default
  `UPSTREAM`.
- NFR-3 Streaming is unchanged: SSE and Streamable-HTTP MCP flow through
  unbuffered (`FlushInterval = -1`), and no policy reads the response body.
  Token counting reads only the final `usage` frame.
- NFR-4 Folder-state store under `USER_DATA_DIR`, with no database. Keys are
  stored as sha256 and compared in constant time.
- NFR-5 Hot-path cost: resolving a key and checking policy with a cached
  credential adds ≤ 2 ms at p99 on the reference box.
- NFR-6 Management writes are audited: who, what, when, and a before/after hash.
- NFR-8 The sim lane's deterministic **token contract** (`input_tokens =
  max(1, bodyBytes/4)`, `output_tokens = max(1, word count)`) stays unchanged;
  parent-repo e2e and OTEL tests assert it.
- NFR-7 Open mode (`OIDC_ISSUER` unset) exists for the management plane in local
  dev only. The subscription lane **always** requires a key.

## 8. Out of scope (v1)

A setup UI or developer portal. XML or expression-language policies. Semantic
caching. Content safety. Response transformation. GraphQL, SOAP and WebSocket
APIs. Billing and invoicing. Multi-region. Load-balanced backend pools. MCP
resources and prompts.

## 9. Decisions and open questions

### Decided 2026-09-23 (Poul)

- **Function map (§4):** accepted as proposed.
- **Names:** Bundle, Subscription, Principal. The product stays
  `pks-agent-gateway`.
- **Vault for a long-running server:** re-release on a TTL shorter than one hour
  (ADR 0002, option A).
- **MCP auth:** Keycloak or Entra ID acts as the authorization server that
  claude.ai redirects to. The gateway is **not** an authorization server; it only
  validates tokens as a protected resource (FR-6a).
- **Expired subscriptions:** postponed. The graph keeps `expired` as terminal
  for now.
- **The legacy `project` label:** retire it. It is the grouping
  `gateway-cli project` writes into `OTEL_RESOURCE_ATTRIBUTES=project=…` today,
  so a client can claim to be in any project. Subscription attribution replaces
  it. `/api/projects` stays read-only until the subscription lane ships, then it
  is removed.

### Still open

3. **Key presentation.** Is `x-api-key` enough for Anthropic SDK clients, or do we
   need `ANTHROPIC_AUTH_TOKEN`/bearer as well? What does the Foundry SDK send
   (`api-key`)?
4. **Client registration for claude.ai.** Is Keycloak DCR enough, or do we need a
   pre-registered client (CIMD) per authorization server? This decides the
   Keycloak and Entra setup, not the gateway.
6. **Approval.** Do subscriptions need pks-agent-consent approval in v1 (F9), or
   only when bundles become self-service?
8. **`tools/list` usage.** It is key-gated like `tools/call`. Should it also
   count as a usage record?
9. **Re-using ids.** Can a removed API id be registered again? The graph
   currently says no.

### Choices the product-cli graph forced (see `.product/`)

- An extra `GatewayRequest` aggregate carries the data-plane rules as invariants.
  A decider cannot choose between `RequestAuthorized` and `RequestRejected`.
- Usage recording and expiry are modelled as automated triggers. The real
  implementation records usage inline, and treats expiry as a status that follows
  from `expiresAt`.
- CLI output is modelled as terminal (TUI) UI steps on `sys-gateway-cli`, because
  a product with no GUI still needs read models to feed a view.
- NFR-1/2/3/4/6 become How-side architectural constraints once `product how init`
  exists. Until then only NFR-5 is a quality demand; NFR-1/2 and hashed keys are
  covered by invariants.
- Checks that span aggregates (no subscribing a disabled principal; token-limit
  only on `llm` APIs) are invariants that no decider enforces yet.

## 10. First thin slice (after the decisions)

The Foundry transcribe API with an `entra` credential goes into the `speech`
bundle. One service principal and one human subscription. The transcribe
operation is also exposed as an MCP tool. Usage is recorded per subscription.
`gateway-cli` verbs cover every step. Stories 1–4 and 9 must pass as e2e.
