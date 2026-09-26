# `speech.realtime.transcribe`

Status: experimental v1

`speech.realtime.transcribe` converts a live mono audio stream into partial and
final transcript events. Clients connect to the Gateway and do not know which
cloud or model serves the request.

## Endpoint

```http
GET /v1/speech/realtime
Connection: Upgrade
Upgrade: websocket
Authorization: Bearer <gateway client token>
```

The Gateway opens a second WebSocket to a compatible internal runtime and
bridges messages after authentication and routing. The runtime has its own
service credential; the client credential is never forwarded.

## Starting a session

The first client message must be a UTF-8 JSON text frame:

```json
{
  "type": "session.start",
  "capability": "speech.realtime.transcribe",
  "language": "da",
  "audio": {
    "sampleRate": 24000,
    "channels": 1,
    "encoding": "pcm16"
  }
}
```

Current audio contract:

- signed 16-bit little-endian PCM
- 24,000 samples per second
- one channel
- binary WebSocket frames after `session.start`

`language` is optional. Unsupported formats or capability identifiers end the
session with a `provider.error` event.

## Server events

Server events are UTF-8 JSON text frames. Every event has:

```json
{
  "type": "session.started",
  "sequence": 1,
  "at": "2026-09-26T12:00:00Z"
}
```

`sequence` is monotonically increasing within the connection. `at` is generated
by the runtime in UTC.

### `session.started`

Confirms that the provider session is ready:

```json
{
  "type": "session.started",
  "sequence": 1,
  "at": "2026-09-26T12:00:00Z",
  "capability": "speech.realtime.transcribe",
  "provider": "azure-openai-realtime-transcription"
}
```

The provider value is diagnostic metadata. Clients must not branch their core
behavior on it.

### `transcript.partial`

Contains unstable text that may be replaced by a later partial or final event:

```json
{
  "type": "transcript.partial",
  "sequence": 2,
  "at": "2026-09-26T12:00:01Z",
  "text": "welcome to"
}
```

### `transcript.final`

Contains committed transcript text:

```json
{
  "type": "transcript.final",
  "sequence": 3,
  "at": "2026-09-26T12:00:02Z",
  "text": "Welcome to the Agentics Factory."
}
```

### `provider.error`

Reports a failure after the WebSocket has been accepted:

```json
{
  "type": "provider.error",
  "sequence": 4,
  "at": "2026-09-26T12:00:03Z",
  "message": "transcription provider unavailable"
}
```

The message must be safe for the client and must never contain credentials.

## Lifecycle

- Either side may close the WebSocket to end the session.
- A normal close does not imply that the meeting or recording has ended; it
  only ends this transcription stream.
- Clients should treat partial transcript events as replaceable UI state and
  final events as durable meeting progress.
- Retry and resume semantics are not part of experimental v1. A reconnect opens
  a new transcription session.

## Adapter requirements

An adapter implementing this capability must:

1. advertise `speech.realtime.transcribe` from `GET /v1/capabilities`;
2. accept the session and audio contract above;
3. translate provider events into the defined Agentics event types;
4. obtain provider credentials itself, preferably through workload identity;
5. avoid leaking provider-specific credentials or request shapes;
6. preserve event ordering on each WebSocket connection.

The current `pks-cli-runtime` adapter uses Azure Managed Identity and Azure
OpenAI Realtime Transcription. That provider choice is not part of this
specification.
