# ADR 0004: Runner-hosted upstreams (connectors) over the tunnel protocol

- Status: **Accepted** 2026-09-25
- Date: 2026-09-25
- Related: [ADR 0001](0001-three-lanes.md) (lanes), [ADR 0002](0002-credential-sources.md)
  (credential sources), [ADR 0005](0005-multi-tenant-and-console.md) (tenancy),
  pks-agent-tunnel `docs/protocol.md`

## Context

Some APIs should be published through the gateway without the gateway, or
agentics.dk, ever holding the upstream credential. The motivating case is a
Scaleway API: pks-cli runs on a machine the customer controls, already holds
the Scaleway credentials, and should *serve* the API to subscribers the way an
agentics runner serves jobs.

This is APIM's self-hosted gateway and Azure Relay's hybrid connections: the
serving side dials **out**, so nothing inbound has to be opened on its network.

We already built and run this transport. pks-agent-tunnel is a WSS carrier with
a yamux session on it. Stream 0 carries a JSON `register` frame with named
slots, plus `ping`/`pong` heartbeats. Each inbound request opens a new stream
that starts with one `StreamMeta` JSON line followed by raw HTTP bytes. The
`agent-tunnel host` CLI reconnects with backoff and forwards each slot to a
local `host:port`.

## Decision

1. **The gateway speaks the tunnel protocol unchanged** at
   `GET /connect/v1/control` (WebSocket upgrade). It imports the wire types from
   `github.com/pksorensen/pks-agent-tunnel/src/protocol` rather than copying
   them, so the two cannot drift. An unmodified `agent-tunnel host` is therefore
   a valid connector:

   ```
   agent-tunnel host --server wss://gateway.agentics.dk/connect \
     --owner acme --name scaleway --token gwc_… --http api=127.0.0.1:8900
   ```

   pks-cli wraps this later (`pks gateway connect`), next to whatever local
   proxy injects the Scaleway key. The wrapper is convenience only; the protocol
   is the contract.

2. **A Connector is an owner-scoped entity** managed through `/api/v1`, with the
   same lifecycle shape as a Subscription:
   - it has an id and a name;
   - its token, `gwc_<connectorId>_<secret>`, is shown once and stored as sha256
     in two slots;
   - it can be `disabled`.

   The `register` frame's `token` must match. The frame's `owner` is used
   **only to locate the owner's store** — the token hash decides, so a wrong or
   foreign owner simply fails authentication. The frame's `tunnel` field is
   ignored: the session binds to the connector the token names. A token can
   only ever serve its own owner's APIs. (Keys stay `gwc_<connector>_<secret>`,
   as with `gwk_`; the owner is not encoded in the token.)

3. **An API can point at a connector:** `upstream: "runner://<connector>/<slot>[/base/path]"`.
   Such an API **must not** carry a `credential`, because the credential lives
   with the runner. The lane is otherwise unchanged:
   - Bundles, Subscriptions, key stripping, the operation allow-list, MCP tools
     and usage all apply as they do today;
   - the transport is a yamux stream instead of a TCP dial.

4. **Presence:**
   - Several sessions may register the same connector. Requests are spread
     round-robin across the live sessions, which gives failover for free.
   - When no session is live, the API answers `503 connector offline` locally,
     which is never a passthrough.
   - Liveness (`connected`, `sessions`, `lastSeen`) is readable through the
     management API, so the console can show it.

5. **Rotation and teardown.**
   - Disabling or deleting a connector closes all of its sessions.
   - Regenerating one token slot closes only the sessions that slot
     authenticated, so the other slot keeps serving during a rotation.
   - Requests reach the runner with `Host: localhost` and the API's base path,
     so a local service that routes by virtual host must accept `localhost`.

6. **The request context travels with the request.** The gateway adds the
   header `X-Gateway-Context: sub=<id>; principal=<id>; bundle=<id>; op=<id>` so
   the runner can audit and apply its own policy. It never includes the
   subscriber's key, which is stripped as before.

## Consequences

- For runner-hosted APIs, the upstream secret never leaves the customer's
  machine. This is the strongest form of "agentics.dk holds no credentials".
- The gateway becomes a tunnel server for its own connectors only. It never
  exposes slots on public subdomains: traffic reaches a connector **only**
  through an API in a Bundle, behind a subscription key.
- Streaming works as on direct upstreams, because the reverse proxy runs with
  `FlushInterval=-1` over the yamux stream.
- The dependency on the tunnel's protocol module is pinned by pseudo-version.
  A breaking protocol change must bump both repos together.
- The runner trusts the gateway to have authorised the call. Defence in depth
  (the runner enforcing its own allow-list from `X-Gateway-Context`) is
  recommended, but the gateway does not require it.
