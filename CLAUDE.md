# pks-agent-gateway — Development Guide

A transparent streaming reverse proxy to `api.anthropic.com`, so Claude Code can
reach Anthropic from networks that block it. Deployed as an Azure Container App;
set as `ANTHROPIC_BASE_URL`.

This sub-project is self-contained and slated for its own repo — keep all docs,
ADRs, and configs inside this folder; do not touch the parent repo's shared docs.

## Layout

```
src/gateway/        Go single-binary reverse proxy (stdlib only, no deps)
  main.go           the whole proxy
  Dockerfile        multi-stage alpine build, builds from project root context
README.md           usage + Azure Container Apps deploy
```

## Build & test

```bash
cd src/gateway
go build -o gateway . && PORT=8080 ./gateway
```

Smoke test: a bogus `x-api-key` POST to `/v1/messages` should return Anthropic's
real `401 authentication_error` (proves the hop reaches upstream and passes auth
through). See README.

## Design notes

- **Dumb on purpose.** No auth plane of its own; the caller's `x-api-key` flows
  through untouched. Only the `Host` header is rewritten to the upstream.
- **Streaming.** `proxy.FlushInterval = -1` is required for SSE token streaming;
  do not remove it.
- **Open-proxy guard.** Optional `GATEWAY_TOKEN` → require `X-Gateway-Token`
  header. Wire it via Claude Code's `ANTHROPIC_CUSTOM_HEADERS`.

## Roadmap (later, if needed)

- Aspire hosting drop-in (mirror `pks-agent-tunnel`'s `src/aspire/`).
- Optional request/usage logging, rate limiting, multi-upstream (Bedrock/Vertex).
