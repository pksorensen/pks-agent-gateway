# ADR 0007: WebSocket operations on the subscription lane

- Status: **Accepted** 2026-10-17
- Date: 2026-10-17
- Extends: [ADR 0006](0006-entra-jwt-callers-and-entra-cert-upstreams.md)
  (per-principal limits and audio metering), [ADR 0001](0001-three-lanes.md)
  (the subscription lane)
- Deploy runbook: [docs/deploy/arvo-coolify.md](../deploy/arvo-coolify.md)

## Context

The vandvaerk site wants live transcription with `MAI-Transcribe-2-Streaming`.
The upstream is
`GET wss://contextand-cs-foundry.services.ai.azure.com/mai/v1/realtime?intent=transcription`,
authenticated with the same Entra bearer as the other Foundry APIs (scope
`https://cognitiveservices.azure.com/.default`). Frames are JSON, close to
OpenAI Realtime, in both directions. Sessions can last up to 1 h upstream.

The older realtime-speech bridge (`GATEWAY_REALTIME_SPEECH_ENABLED`) is a fixed
route to one runtime with no principal, subscription or limit. It is the wrong
place for this. The subscription lane already has callers, credentials, limits
and usage. What it lacked was a way to declare an operation that upgrades.

## Decision

1. **An operation can be `"websocket": true`.** It must be `GET`. Such an
   operation accepts only a WebSocket upgrade (`Upgrade: websocket` plus
   `Connection: upgrade`). Anything else gets **426**, and no other method
   matches it. A WebSocket operation is never an MCP tool.
2. **The proxy is the stdlib's.** The upgrade request goes through the same
   `httputil.ReverseProxy` and `Rewrite` as every operation:
   - the credential is injected;
   - the caller's `Authorization`, `Api-Key` and cookies are stripped;
   - the query is kept.

   ReverseProxy relays the 101, hijacks the caller connection and splices the
   two connections byte for byte. Frames are never parsed, buffered or logged.
   For an `https` upstream, `net/http` forces HTTP/1.1 for a websocket upgrade
   (`Request.requiresHTTP1`), so ALPN never negotiates h2 on that connection.
   **A non-WebSocket operation never switches protocols:** `Rewrite` drops
   `Upgrade` and `Connection`, which ReverseProxy would otherwise re-add.
3. **Admission comes before minting or dialing.** In order:
   - authentication (**401** / **403**);
   - one request against `requestsPerMinute`;
   - a free slot under `limits.maxStreams` (default **2**, -1 = no cap);
   - for a metered operation, some audio quota left today.

   A refusal is **429** with `Retry-After`. Because all of this happens before
   `credentialFor`, no upstream token is minted for a refused session.
4. **Sessions are bounded.** `maxSessionSeconds` on the operation defaults to
   300. For a metered operation the bound is also capped by the audio seconds
   left today, so a session cannot outrun the quota. The bound is enforced by
   cancelling the request context. In Go 1.25, `handleUpgradeResponse` runs a
   goroutine that closes the upstream connection on `req.Context().Done()`
   (golang/go#35559). The upstream→caller copy then fails and the deferred
   `conn.Close()` closes the caller. The caller sees an abnormal close (1006),
   not a close frame, because the gateway does not speak the frame protocol.
5. **Metering is wall-clock time.** With `"meter": "audio"`, a session is
   charged `ceil(open duration)` seconds when it ends: a started second counts
   as a second. Only sessions that got a 101 are charged. The usage record has
   status 101, `durationMs`, `audioSeconds`, and `bytesIn` / `bytesOut`
   counted on the hijacked connection (frame bytes, excluding the handshake).

## Consequences

- A long session is charged only when it ends. Concurrent sessions may together
  overrun the day's quota by up to `maxStreams × maxSessionSeconds`. That
  bound is known, and it is why both have defaults.
- The cut at `maxSessionSeconds` is a hard close. A client that needs a clean
  stop should send `input_audio_buffer.commit` and close before the bound.
  Callers learn the bound from the runbook, not from the protocol.
- Browsers cannot set `Authorization` on a WebSocket. Callers are server-side
  workloads with an Entra token or a key, as for the other APIs.
- Runner (`runner://`) upstreams take the same path. Their transport returns a
  writable body for 101, but this is untested.

## Not done

- No frame inspection: there is no per-message audio accounting, no schema
  check, and no close frame on cut.
- No idle timeout separate from the session bound.
