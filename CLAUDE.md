# pks-agent-gateway — Development Guide

A transparent streaming reverse proxy to `api.anthropic.com`, so Claude Code can
reach Anthropic from networks that block it. Deployed as an Azure Container App;
set as `ANTHROPIC_BASE_URL`.

This sub-project is self-contained and slated for its own repo — keep all docs,
ADRs, and configs inside this folder; do not touch the parent repo's shared docs.

## Layout

```
src/gateway/        Go single binary (stdlib + go-oidc), flat package main
  main.go           env + mux wiring (routes, auth middleware, sim gate)
  proxy.go          reverse proxy — FlushInterval=-1, do not touch
  auth.go/roles.go  OIDC Bearer middleware (OIDC_ISSUER unset = open dev mode)
  store.go          file store: {USER_DATA_DIR}/owners/{owner}/projects/...
  otel.go/api.go    OTEL ingest + /api/projects management plane
  sim.go            LLM simulator: sim-key gate, sessions, token contract
  sim_sse.go        shape-faithful Messages API SSE/JSON emitter
  scenario.go       scripted scenario engine + built-ins
  cassette.go       record (tee on passthrough) + replay engines
  testbench_api.go  /api/testbench subtree (own inner mux — see below)
  store_testbench.go  scenarios/cassettes storage under owners/{owner}/testbench/
  Dockerfile        multi-stage alpine build, builds from project root context
  gw_*.go           subscription lane: store, keys, credentials, lane, MCP,
                    /api/v1 management plane, OpenAPI import, wiring,
                    tenancy (owners), connectors (tunnel-protocol runners)
src/cli/            gateway-cli (cobra): login, project, env, stats +
                    owner/connector/api/op/mcp/bundle/principal/sub/cred/usage/catalog
scripts/smoke.sh    real-process end-to-end check of the subscription lane
README.md           usage, simulator/test-bench docs, Azure Container Apps deploy
```

## Build & test

```bash
cd src/gateway
go build -o gateway . && go test ./...
GATEWAY_SIM_ENABLED=1 PORT=8080 ./gateway
```

Smoke test: a bogus `x-api-key` POST to `/v1/messages` should return Anthropic's
real `401 authentication_error` (proves the hop reaches upstream and passes auth
through). See README.

## Design notes

- **Three lanes (ADR 0001).** The *passthrough* lane (catch-all) is dumb on
  purpose: the caller's `x-api-key` flows through untouched, only `Host` is
  rewritten. The *sim* lane never reaches upstream. The *subscription* lane
  (`/apis/`, `/mcp/`, `gw_*.go`) is the ONLY one that rewrites credentials: it
  strips the `gwk_` key and injects the API's credential. `gatewayKeyGuard`
  makes a `gwk_` or `gwc_` token on the catch-all a 401 — keep it wrapping the proxy.
- **Multi-tenant subscription lane (ADR 0005).** Every lookup goes through
  `GatewayRoot.Owner(owner)` (the only place the owner segment is validated) and
  the owner is in every URL. Management handlers get their store from
  `ownerStore(r)`, set by `withOwner` after the owner-admin check — never reach
  for a store another way. Legacy passthrough/sim/OTEL stay on `GATEWAY_OWNER`.
- **Connectors (ADR 0004).** `/connect/v1/control` speaks pks-agent-tunnel's
  protocol unchanged (wire types imported, not copied). The frame's `owner` only
  locates the store; the token hash decides. A `runner://` API must not carry a
  `credential`; `X-Gateway-Context` is stripped from clients and set only toward
  runners. Every data-plane path that reaches an upstream (`ServeAPI` AND MCP
  `callAPITool`) must go through `Lane.upstreamTarget`.
- **Subscription-lane invariants** (`gw_lane_test.go` enforces them): undeclared
  operations are a local 404, never forwarded; credential values never appear
  in responses, logs, usage or on disk in plaintext (canary test); keys are
  stored only as sha256; key ids never contain `_` (the secret may — parse on
  the first `_`). Proxy error handlers must not echo `err` (query injection puts
  the secret in the URL). `scripts/smoke.sh` is the real-process end-to-end check.
- **Streaming.** `proxy.FlushInterval = -1` is required for SSE token streaming;
  do not remove it.
- **Open-proxy guard.** Optional `GATEWAY_TOKEN` → require `X-Gateway-Token`
  header. Wire it via Claude Code's `ANTHROPIC_CUSTOM_HEADERS`.

### Simulator invariants (read before touching sim*.go)

- **Sim keys never reach upstream.** `sim.Gate` wraps the proxy catch-all; the
  sim branch has no code path to `next`, even with `GATEWAY_SIM_ENABLED` off
  (off = local 403, not passthrough). `TestGateNeverProxiesSimKeys` enforces
  this with a failing-next stub — keep it green.
- **Playback is sequence-based, matchers are drift assertions.** Claude Code's
  system prompts embed dates/cwd/git noise; routing steps by content match
  would be nondeterministic. Never turn `match` into a router.
- **Side-call lane:** Claude Code's session-title/summary calls use the MAIN
  model but advertise no `tools` (verified against claude 2.1.207) — toolless
  requests and haiku-model requests are side-calls and must not consume main
  scenario steps. If a future Claude Code changes this shape, fix the
  classifier in `Scenario.isSidecall` + `replayIsSidecall`, not the scenarios.
- **Catch-all shadowing:** any endpoint not registered on the mux falls through
  to the proxy and leaks the request upstream. New management namespaces get
  their own subtree handler (like `/api/testbench/`) — do not extend api.go's
  prefix/suffix switch.
- **Token contract** (`input = bodyBytes/4`, `output = word count`) is asserted
  by e2e/OTEL tests — changing it is a breaking change for tests/e2e in the
  parent repo.

## Roadmap (later, if needed)

- Aspire hosting drop-in (mirror `pks-agent-tunnel`'s `src/aspire/`).
- Optional request/usage logging, rate limiting, multi-upstream (Bedrock/Vertex).
