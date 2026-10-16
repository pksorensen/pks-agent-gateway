# Deploy: internal AI gateway on the arvo Coolify box

This deploys the gateway as a **host-internal** service, which means:

- no published ports and no Traefik labels;
- reachable only as `http://ai-gateway:8080` on the `arvo-works` docker network;
- persistent state on a volume at `/data`.

The first caller is `gjelvand-map`, which uses Danish speech-to-text from Azure
Foundry. The design is in [ADR 0006](../adr/0006-entra-jwt-callers-and-entra-cert-upstreams.md).

> Use an image **newer than 1.2.0**. `entra-cert`, `entra-jwt`, limits and
> `GATEWAY_MODE` arrive in the release after it.

## Container

```yaml
# Coolify "Docker Compose" resource. Leave every domain field EMPTY: a domain
# makes Coolify add Traefik labels, which would expose the gateway.
services:
  ai-gateway:
    image: registry.agentics.dk/agentics/pks-agent-gateway:<version>
    restart: unless-stopped
    # no "ports:" — nothing is published on the host
    environment:
      GATEWAY_MODE: prod                     # refuses to start without OIDC_ISSUER
      OIDC_ISSUER: https://<keycloak>/realms/<realm>   # admin API auth (GatewayAdmin role)
      GATEWAY_PASSTHROUGH: "off"             # no Anthropic passthrough on this box
      # GATEWAY_SIM_ENABLED / GATEWAY_REALTIME_SPEECH_ENABLED: leave unset (off)
      ENTRA_JWT_TENANT: <issuer tenant GUID> # where callers + the gateway API app live
      ENTRA_JWT_AUDIENCES: <gateway API app client id GUID>
      # USER_DATA_DIR=/data and GATEWAY_IDENTITY_DIR=/data/identity are the defaults
    volumes:
      - ai-gateway-data:/data
    networks:
      arvo-works:
        aliases: [ai-gateway]

networks:
  arvo-works:
    external: true

volumes:
  ai-gateway-data:
```

The plain-docker equivalent:

```bash
docker run -d --name ai-gateway --restart unless-stopped \
  --network arvo-works --network-alias ai-gateway \
  -v ai-gateway-data:/data \
  -e GATEWAY_MODE=prod -e OIDC_ISSUER=https://<keycloak>/realms/<realm> \
  -e GATEWAY_PASSTHROUGH=off \
  -e ENTRA_JWT_TENANT=<issuer-tenant-guid> -e ENTRA_JWT_AUDIENCES=<gateway-api-app-guid> \
  registry.agentics.dk/agentics/pks-agent-gateway:<version>
```

The startup log should contain this line:

```
gateway mode=prod management-auth=OIDC passthrough=off sim=off realtime-speech=off
```

**Outbound access needed:**

- the Keycloak issuer (discovery runs at boot, and the gateway exits if it fails);
- `login.microsoftonline.com` (token minting and JWKS);
- the Foundry host.

No inbound access is needed.

## 1. Gateway identity (once, on the box)

```bash
# Coolify suffixes container names, so find it by name, or run a one-off on the volume:
docker exec "$(docker ps -qf name=ai-gateway)" /app/gateway identity init --dir /data/identity
# docker run --rm -v <volume>:/data registry.agentics.dk/agentics/pks-agent-gateway:<version> identity init --dir /data/identity
```

The command prints the thumbprint, `x5t` and a `keyCredentials` entry. Running it
again only re-prints them; it never overwrites. The private key stays in
`/data/identity/key.pem` with mode `0600`.

## 2. Azure side (by an operator; the gateway never touches a tenant)

**Upstream tenant (Foundry):**

1. Create an app registration for the gateway, or reuse one.
2. Add the printed certificate to its `keyCredentials`.
   `PATCH /applications/{id}` replaces the whole list, so include any existing
   entries.
3. Grant its service principal a Speech data-plane role on the Foundry resource.
   `Cognitive Services Speech User` is the narrowest role.

**Issuer tenant (callers):**

1. Create the **gateway API** app. Set `api.requestedAccessTokenVersion = 2` in
   its manifest. Without this, Entra issues v1 tokens, which the gateway refuses.
   Its client id is `ENTRA_JWT_AUDIENCES`.
2. Give each caller (e.g. `gjelvand-map`) its own app registration with its own
   certificate credential. The caller requests a token with
   `scope=<gateway-api-app-id>/.default` and `grant_type=client_credentials`,
   authenticating with its client assertion.

## 3. Register the speech API, credential and principal

Run these with a `GatewayAdmin` access token from the Keycloak realm. The
gateway is not exposed, so call it from inside the network, for example with
`docker run --rm --network arvo-works curlimages/curl …`, or use `gateway-cli`
with `GATEWAY_URL=http://ai-gateway:8080`.

```bash
B=http://ai-gateway:8080/api/v1; O=arvo; H=(-H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json')

# owner (first claimer becomes its admin)
curl "${H[@]}" -X POST $B/owners -d '{"id":"arvo"}'

# upstream credential: the gateway's certificate, Foundry's tenant (realm GatewayAdmin only)
curl "${H[@]}" -X POST $B/owners/$O/credentials -d '{
  "id": "foundry-speech", "source": "entra-cert",
  "tenant": "<upstream tenant GUID>", "clientId": "<gateway upstream app client id>",
  "scope": "https://cognitiveservices.azure.com/.default",
  "keyFile": "key.pem", "certFile": "cert.pem"}'            # inject defaults to Bearer

# the API: one declared operation, colon path, metered as audio, 6 MiB cap
curl "${H[@]}" -X POST $B/owners/$O/apis -d '{
  "id": "speech", "name": "Azure Speech (Foundry)",
  "upstream": "https://contextand-cs-foundry-us-east.cognitiveservices.azure.com",
  "credential": "foundry-speech",
  "operations": [{"id": "transcribe", "method": "POST",
    "path": "/speechtotext/transcriptions:transcribe",
    "summary": "Fast transcription (multipart: audio + definition)",
    "meter": "audio", "maxBodyBytes": 6291456}]}'

curl "${H[@]}" -X POST $B/owners/$O/bundles -d '{"id":"speech","apis":["speech"]}'

# the caller: its Entra app id (the token's azp) + limits
curl "${H[@]}" -X POST $B/owners/$O/principals -d '{
  "id": "gjelvand-map", "kind": "service", "appId": "<gjelvand-map app client id>",
  "limits": {"requestsPerMinute": 30, "audioSecondsPerDay": 3600}}'

# keyless: no gwk_ key exists for this subscription
curl "${H[@]}" -X POST $B/owners/$O/subscriptions -d '{"bundle":"speech","principal":"gjelvand-map","keyless":true}'
```

The same steps with `gateway-cli` (`--owner arvo` or `GATEWAY_OWNER=arvo`):

```bash
gateway-cli owner claim arvo
gateway-cli cred create foundry-speech --source entra-cert --tenant <upstream-tenant> \
  --client-id <gateway-upstream-app> --scope https://cognitiveservices.azure.com/.default \
  --key-file key.pem --cert-file cert.pem --bearer
gateway-cli api create speech --upstream https://contextand-cs-foundry-us-east.cognitiveservices.azure.com \
  --credential foundry-speech
gateway-cli op put speech transcribe POST /speechtotext/transcriptions:transcribe --meter audio --max-body-bytes 6291456
gateway-cli bundle create speech --apis speech
gateway-cli principal create gjelvand-map --app-id <gjelvand-map-app> --rpm 30 --audio-seconds-per-day 3600
gateway-cli sub create --bundle speech --principal gjelvand-map --keyless
```

## 4. The caller's request

```bash
curl -X POST "http://ai-gateway:8080/apis/arvo/speech/speechtotext/transcriptions:transcribe?api-version=2025-10-15" \
  -H "Authorization: Bearer $ENTRA_V2_APP_TOKEN" \
  -F audio=@clip.wav \
  -F 'definition={"locales":["da"],"enhancedMode":{"enabled":true,"model":"mai-transcribe-1.5","phraseList":["…"]}}'
```

The gateway responds to failures as follows:

| Status | Meaning |
|---|---|
| 401 | Bad or expired token |
| 403 | Unknown `appId`, or no subscription covers the API |
| 413 | Body over the cap |
| 429 | Limit reached; `Retry-After` gives the wait |

Usage records (`GET $B/owners/arvo/usage`) attribute each call to `gjelvand-map`
with `bytesIn` and `audioSeconds`. Bodies are never logged.

## Rotation and backup

- The certificate is valid for 2 years. The rotation steps are in ADR 0006 §1:
  generate a new pair in `next/`, register both certificates, swap the files in
  place, `cred rotate`, then remove the old certificate.
- `/data` now holds the gateway's private key. Back up the volume as a secret,
  or treat the key as disposable and re-register a new certificate after a loss.
