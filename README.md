# pks-agent-gateway

A transparent, streaming reverse proxy to the Anthropic API.

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
