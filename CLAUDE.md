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
src/cli/            gateway-cli (cobra): login, project list/create, env, stats
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

- **Dumb proxy on purpose.** No auth plane on the proxy path; the caller's
  `x-api-key` flows through untouched. Only the `Host` header is rewritten.
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
