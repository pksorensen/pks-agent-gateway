# Gateway capabilities

The Agentics Gateway is a capability gateway, not only a transparent reverse
proxy. A capability gives clients one stable Agentics protocol while a runtime
adapter translates that protocol to a concrete provider.

```text
client component
  -> Agentics Gateway capability contract
    -> registered runtime adapter
      -> cloud, model, tool, or local service
```

The separation is intentional:

- clients depend on capability identifiers and Agentics wire contracts;
- the Gateway authenticates, applies policy, selects a runtime, and bridges the
  stream;
- runtimes own provider credentials, provider SDKs, and protocol translation;
- providers remain replaceable without changing clients.

The transparent passthrough and subscription lanes documented elsewhere remain
available. Capability endpoints are a separate lane with Agentics-owned wire
semantics.

## Contract rules

1. Capability identifiers are stable, lowercase, dot-separated names such as
   `speech.realtime.transcribe`.
2. A client never sends provider credentials or provider-specific request
   shapes through a capability endpoint.
3. The Gateway may route a capability to any compatible runtime. Runtime URLs,
   provider names, deployment names, and cloud identities are implementation
   details.
4. Runtime adapters expose `GET /v1/capabilities` and advertise the exact
   capability identifiers they implement.
5. Capability protocol changes must be backward compatible within a protocol
   version. Breaking changes require a new versioned endpoint or capability.
6. Provider errors are translated to the capability's error events. Raw
   credentials and provider responses must not be exposed to clients.

## Current capabilities

| Capability | Gateway endpoint | Runtime transport | Specification |
|---|---|---|---|
| `speech.realtime.transcribe` | `WS /v1/speech/realtime` | WebSocket | [Realtime speech transcription](speech.realtime.transcribe.md) |

## Runtime discovery

A runtime reports its implementations from `GET /v1/capabilities`:

```json
{
  "capabilities": [
    {
      "id": "speech.realtime.transcribe",
      "transport": "websocket",
      "provider": "azure-openai-realtime-transcription",
      "audio": {
        "sampleRate": 24000,
        "channels": 1,
        "encoding": "pcm16"
      }
    }
  ]
}
```

The first implementation is hosted by `pks-cli-runtime`. This is an adapter
host, not part of the public client contract; another implementation can replace
it if it advertises and follows the same capability specification.
