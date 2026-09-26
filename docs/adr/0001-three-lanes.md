# ADR 0001: Three lanes: passthrough, sim, subscription

- Status: **Proposed**
- Date: 2026-09-23
- Context doc: [PRD 0001](../prd/0001-api-gateway.md)

## Context

v1 of the gateway is a dumb proxy on purpose. The caller's `x-api-key` or
`Authorization` header reaches `UPSTREAM` untouched, and the gateway never stores
or rewrites a credential. The LLM simulator adds a second path: any key starting
with `sim-` is answered locally and has no code path to the proxy
(`TestGateNeverProxiesSimKeys`).

PRD 0001 needs the opposite behaviour for gateway-issued keys: check the key,
strip it, and **inject a different credential** before forwarding to a
**per-API upstream**. If that behaviour were folded into the existing proxy, it
would put "sometimes rewrite credentials" into a path whose whole guarantee is
"never rewrite credentials".

## Decision

Incoming data-plane traffic is split into three **lanes**. Each request is
classified once, before any handler runs, and lanes never fall through into each
other.

| Lane | Selected by | Upstream | Credential handling |
|---|---|---|---|
| **sim** | the presented key has the `sim-` prefix | none (local) | none. Off means a local 403 |
| **subscription** | the presented key has the gateway key prefix (`gwk_`, final prefix to be decided), **or** the path is under a registered API prefix or `/mcp/` | the matched API's `upstream` | key checked, then **stripped** — or, on `/mcp/`, an OAuth bearer JWT from Keycloak or Entra validated as a protected resource (PRD FR-6a); the gateway issues no tokens. Then the credential from ADR 0002 is injected |
| **passthrough** | anything else on the legacy catch-all | `UPSTREAM` (Anthropic) | untouched, exactly as in v1 |

Rules:

1. **Classification is by key prefix first, then by route.** A `gwk_` key on the
   catch-all is a 401, not a passthrough. A request under an API prefix or
   `/mcp/` with no valid key is a 401, never a passthrough. This closes the
   catch-all shadowing hole for the new routes.
2. The subscription lane gets its **own subtree handlers** (`/{apiPrefix}/…`
   registered from the store, `/mcp/{server}`), in line with the existing rule
   for `/api/testbench/`. It never shares a switch with `api.go`.
3. An injected credential exists only in the subscription lane's outbound request
   object. Passthrough code does not import the credential resolver. A test with a
   canary credential asserts that it never appears in passthrough, sim, logs,
   cassettes or error bodies.
4. The streaming invariant holds in every lane: `FlushInterval = -1`, and nothing
   reads the response body except the token-usage tap on `llm` APIs, which reads
   the stream without buffering it.
5. Passthrough may later be **disabled per deployment**
   (`GATEWAY_PASSTHROUGH=off`) once the Anthropic proxy is registered as an `llm`
   API in the subscription lane. It stays on by default, so existing users see no
   change.

## Consequences

- Existing Claude Code and e2e users are unaffected (PRD story 9).
- The gateway key prefix becomes a reserved namespace, alongside `sim-`.
- There are three test fences: sim never reaches an upstream; passthrough never
  carries an injected credential; subscription traffic never reaches the default
  `UPSTREAM`.
- CLAUDE.md's "dumb proxy" note must be reworded to "the *passthrough lane* is
  dumb on purpose" when this lands.
