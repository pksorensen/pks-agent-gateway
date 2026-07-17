# pks-agent-gateway

A transparent, streaming reverse proxy to the Anthropic API — with a built-in
**LLM simulator / test bench** (see below) for exercising Claude Code harnesses
without a real subscription.

**Why:** on networks where `api.anthropic.com` is blocked, deploy this gateway
somewhere reachable (an Azure Container App), point Claude Code's
`ANTHROPIC_BASE_URL` at it, and every request — including streaming token
responses — is forwarded upstream unchanged. Your own `x-api-key` rides along in
the request; the gateway never stores or rewrites it.

```
Claude Code ──HTTPS──▶ pks-agent-gateway (Azure) ──HTTPS──▶ api.anthropic.com
            (blocked path avoided)        (allowed path)
```

## What it does (v1)

- Forwards `POST /v1/messages` and every other path/verb verbatim.
- Streams SSE responses byte-for-byte (`FlushInterval = -1`).
- Sets the upstream `Host`; passes through `x-api-key`, `authorization`,
  `anthropic-version`, etc.
- `GET /healthz` → `ok` (for container ingress probes).
- Optional `GATEWAY_TOKEN` shared secret so the public URL isn't an open proxy.

## Config

| Env | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | Listen port |
| `UPSTREAM` | `https://api.anthropic.com` | Where to forward |
| `GATEWAY_TOKEN` | _(unset)_ | If set, callers must send `X-Gateway-Token: <token>` |
| `GATEWAY_SIM_ENABLED` | _(unset)_ | `1` enables the LLM simulator (below). `sim-` keys are ALWAYS intercepted — when disabled they get a local 403, never the proxy |
| `USER_DATA_DIR` | `./data` | File store root (projects, OTEL, testbench scenarios/cassettes) |
| `GATEWAY_OWNER` | `default` | Owner segment of the store layout |
| `OIDC_ISSUER` | _(unset)_ | OIDC issuer for the `/api` management plane; unset = open dev mode |

## LLM simulator ("test bench")

With `GATEWAY_SIM_ENABLED=1`, the gateway serves a deterministic, shape-faithful
Anthropic Messages API for any request whose API key starts with `sim-` — one
instance serves real passthrough traffic and simulated traffic side by side.
Built for e2e-testing agent harnesses (agentics.dk runners, vibecast, Claude
Code itself) without a subscription; verified against a real `claude -p`
session including a scripted `tool_use` → tool execution → `end_turn` loop.

**Directives** (`ANTHROPIC_API_KEY` form / `X-Gateway-Sim` header form):

| Key | Header | Behavior |
|---|---|---|
| `sim-echo` | `echo` | Replies with the last user text, `end_turn`, forever |
| `sim-scenario:<name>` | `scenario:<name>` | Plays a scripted scenario (built-in or stored) |
| `sim-replay:<cassette>` | `replay:<cassette>` | Replays a recorded cassette sequence-based |

```bash
GATEWAY_SIM_ENABLED=1 ./gateway &

# Streaming curl smoke:
curl -sN localhost:8080/v1/messages -H 'x-api-key: sim-echo' \
  -H 'content-type: application/json' \
  -d '{"model":"claude-opus-4-8","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello sim"}]}'

# Real Claude Code against the sim (also suppresses the login gate):
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY=sim-scenario:tool-loop-then-stop \
  claude -p "do the task" --dangerously-skip-permissions
```

**Built-in scenarios** (zero setup; a stored file with the same name overrides):

- `echo` — last user text, `end_turn`, forever.
- `tool-loop-then-stop` — calls the tool matching `stop_broadcast`
  (`mcp__vibecast__stop_broadcast` when the vibecast plugin is loaded), then
  `end_turn` after the tool_result — cleanly completes an ALP task job.
- `rate-limit-then-succeed` — 429 with `retry-after: 1`, then success (the
  SDK's auto-retry advances the sequence).

**Scenario documents** are JSON, stored at
`{USER_DATA_DIR}/owners/{owner}/testbench/scenarios/{name}.json` and managed via
`PUT /api/testbench/scenarios/{name}`:

```jsonc
{
  "name": "my-scenario",                     // slug, must match the URL name
  "onExhausted": "end_turn",                 // end_turn (default) | last | error
  "sidecall": {                              // utility-call lane (see below)
    "modelRegex": "(?i)haiku",               // default
    "text": "ok",                            // canned side-call reply
    "toolless": true                         // default: toolless requests are side-calls
  },
  "steps": [
    {
      "match": {                             // OPTIONAL drift assertions — a mismatch
        "requestIndex": 0,                   // logs a warning on the session, but the
        "lastMessageContains": "…",          // step still serves (playback stays
        "modelRegex": "(?i)opus"             // sequence-based and deterministic)
      },
      "repeat": 1,                           // requests served by this step; -1 = forever
      "response": {
        "message": {                         // exactly one of message | error
          "content": [
            { "type": "thinking", "thinking": "…" },
            { "type": "text", "text": "Working on it: $lastUserText" },
            { "type": "tool_use", "name": "$toolMatching:stop_broadcast",
              "input": { "message": "done", "conclusion": "success" } }
          ],
          "stopReason": "tool_use",          // auto-derived if omitted
          "usage": { "inputTokens": 0, "outputTokens": 0 },   // 0 = deterministic auto
          "chunkSize": 16, "delayMs": 0, "pingEveryNChunks": 0 // streaming pacing
        }
      }
    },
    { "response": { "error": {
        "status": 429, "message": "simulated rate limit",
        "retryAfterSeconds": 1,
        "afterChunks": 0                     // >0 on a streaming request = mid-stream SSE error
    } } }
  ]
}
```

Substitutions in `text`/`thinking`: `$lastUserText`, `$model`, `$requestIndex`.
In `tool_use.name`: `$toolMatching:<substr>` resolves to the first tool the
request advertises whose name contains the substring (falls back to the
substring).

**Sessions & lanes.** Playback is sequence-based per session (identity:
`X-Gateway-Sim-Session` header → `metadata.user_id` → hash of the first user
message). Utility calls — Claude Code's session-title/topic requests, which use
the MAIN model but advertise **no tools**, plus anything matching the haiku
`modelRegex` — are served from a separate side-call lane and never consume
scenario steps. Inspect live sessions (cursors + drift warnings) via
`GET /api/testbench/sessions`.

**Deterministic token contract** (assertable in OTEL/stats tests):
`input_tokens = max(1, requestBodyBytes/4)`; `output_tokens = max(1, word count
of emitted blocks)`; `count_tokens` applies the input rule to its body.

**Record & replay.** A passthrough request (real key) carrying
`X-Gateway-Record: <name>` — wire it with
`ANTHROPIC_CUSTOM_HEADERS="X-Gateway-Record: my-tape"` — is proxied normally
while the exchange (request JSON, loose fingerprint, raw response bytes) is
appended to `testbench/cassettes/<name>.jsonl`. Credential headers are never
written. `sim-replay:<name>` then serves the Nth recorded main-lane exchange to
the Nth main-lane request; fingerprint mismatches (model, message count, roles)
log drift warnings but never hard-fail. Stream shape adapts both ways
(recorded SSE ↔ requested JSON).

**Management API** (`/api/testbench/…`, same auth as `/api/projects`; every
route answers 403 while the sim is disabled):

```
GET|PUT|DELETE /api/testbench/scenarios[/{name}]
GET|DELETE     /api/testbench/cassettes[/{name}]      (?full=1 includes bodies)
GET|DELETE     /api/testbench/sessions[/{key}]
```

**Guarantee:** a request carrying a `sim-` key is never forwarded upstream —
the sim branch has no code path to the proxy, enforced by a unit test whose
fake upstream fails the build if reached. Prompts in test traffic stay local.

## Run locally

```bash
cd src/gateway
go build -o gateway . && PORT=8080 ./gateway

# In another shell — point Claude Code at it:
ANTHROPIC_BASE_URL=http://localhost:8080 claude
```

A quick sanity check (a bogus key should return a real Anthropic 401, proving the
hop works):

```bash
curl -s -X POST localhost:8080/v1/messages \
  -H "x-api-key: sk-ant-bogus" -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -d '{"model":"claude-opus-4-8","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}'
# -> {"type":"error","error":{"type":"authentication_error",...}}
```

## Deploy to Azure Container Apps

`az containerapp up` builds the Dockerfile, pushes to ACR, and creates the app
with external HTTPS ingress in one step.

```bash
az login
RG=pks-agent-gateway
LOC=westeurope
az group create -n $RG -l $LOC

az containerapp up \
  --name pks-agent-gateway \
  --resource-group $RG \
  --location $LOC \
  --source . \
  --ingress external \
  --target-port 8080
```

The command prints the FQDN, e.g.
`https://pks-agent-gateway.<hash>.westeurope.azurecontainerapps.io`.

> The build context is the project root so the Dockerfile can `COPY src/gateway`.
> Run `az containerapp up` from the **`projects/pks-agent-gateway/`** directory
> (where this README lives), or pass `--source projects/pks-agent-gateway`.

### Lock it down (recommended once it works)

```bash
az containerapp update -n pks-agent-gateway -g $RG \
  --set-env-vars GATEWAY_TOKEN=$(openssl rand -hex 16)
```

Then have Claude Code attach the header:

```bash
export ANTHROPIC_BASE_URL=https://pks-agent-gateway.<hash>.azurecontainerapps.io
export ANTHROPIC_CUSTOM_HEADERS="X-Gateway-Token: <the-token>"
claude
```

## Consume the published image (customer path)

CI publishes a **public** image to GitHub Container Registry on every push to
`main` and every `v*` tag:

```
ghcr.io/pksorensen/pks-agent-gateway:latest
```

Because it's public, the customer's Azure Container App pulls it with **no
registry credentials**:

```bash
RG=pks-agent-gateway
az group create -n $RG -l westeurope
az containerapp env create -n gw-env -g $RG -l westeurope

az containerapp create \
  --name pks-agent-gateway \
  --resource-group $RG \
  --environment gw-env \
  --image ghcr.io/pksorensen/pks-agent-gateway:latest \
  --ingress external \
  --target-port 8080 \
  --min-replicas 1
# optional open-proxy guard:
#  --env-vars GATEWAY_TOKEN=secretvalue
```

It prints the FQDN — that's your `ANTHROPIC_BASE_URL`.

## Point Claude Code at the deployed gateway

```bash
export ANTHROPIC_BASE_URL=https://pks-agent-gateway.<hash>.azurecontainerapps.io
export ANTHROPIC_API_KEY=sk-ant-...   # your real key, unchanged
claude
```

If it answers, the block is bypassed. ✅
