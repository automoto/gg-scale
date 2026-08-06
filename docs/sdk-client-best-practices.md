# ggscale SDK HTTP & WebSocket client guide

Use these rules for every official SDK. `MUST`, `SHOULD`, and `MAY` indicate required, recommended, and optional behavior.

## Contract and client lifecycle

- Treat [`openapi.yaml`](../openapi.yaml) as the REST wire contract. Generate operation models from its stable `operationId` values, pin the generator version, and CI-check that regeneration is clean. Keep auth, retries, telemetry, pagination helpers, and ergonomic APIs in a small handwritten runtime.
- Expose one reusable, concurrency-safe client with configurable base URL, user agent (`ggscale-<lang>/<sdk-version>`), timeouts, proxy/TLS settings, retry policy, logger, and transport injection for tests. Reuse connection pools; consume and close response bodies.
- Every call MUST accept cancellation and an overall deadline that includes every attempt and backoff. Also bound connect/TLS/response-header/body waits and response sizes; never wait forever.
- Be strict when encoding requests but tolerant when decoding responses: ignore unknown JSON fields and preserve or expose unknown enum/event values where the language permits.

## HTTP behavior

- Send the tenant key as `Authorization: Bearer <api_key>` and, where required, the player JWT as `X-Session-Token`. Game clients may embed only a **publishable** key; secret keys stay server-side. Store player tokens with platform-secure storage, never put them in URLs, and never forward them across origins on redirect. Refresh from `expires_at` slightly early; serialize rotation and atomically publish the new token pair so concurrent calls cannot reuse an invalidated refresh token. Never automatically retry `/v1/auth/refresh` after an ambiguous transport failure.
- Handle every documented success code, especially bodyless `204` and `304`. Implement cursor iteration using `next_cursor`; support `ETag`/`If-None-Match` for `/v1/config` and `If-Match` for versioned storage writes.
- Return a typed `APIError` containing HTTP status, problem `type`, `title`, `detail`, `instance`, validation `errors`, `X-Request-Id`, `Retry-After`, and a bounded diagnostic body when parsing fails. Branch on status and problem `type`, never human-readable `detail`. Preserve the underlying cause and distinguish cancellation, timeout, DNS/connect/TLS, HTTP, and decode/protocol failures.

### Retry and backoff

| Situation | SDK behavior |
|---|---|
| Temporary connection failure, timeout, `408`, `429`, `502`, `503`, or `504` | Retry only when the operation is safe to replay. |
| `GET`, `HEAD`, `PUT`, or `DELETE` | Replayable by HTTP semantics, provided the request body can be recreated. |
| `POST` or `PATCH` | **Do not retry automatically.** ggscale currently specifies no idempotency key; a lost response can hide a successful mutation. Offer an explicit caller opt-in only when operation semantics make replay safe. |
| Cancellation, TLS/certificate validation, malformed response, or other `4xx` | Return immediately. Fixing credentials may permit one new call, but never loop on `401`/`403`. |

Default to **3 total attempts**. Before retry `n`, use full jitter: `random(0, min(10s, 250ms × 2^n))`. For `429`/`503`, parse both forms of `Retry-After` and wait at least that long, subject to the overall deadline. Stop when the retry/deadline budget is exhausted, make request bodies replayable, and avoid nested retry layers. Defaults MUST be configurable.

## Logging, telemetry, and secrets

- Libraries MUST be silent by default and expose structured log/trace hooks. Generate one `X-Request-Id` per logical call and retain it across attempts. Record the OpenAPI `operationId` or route template (never a raw high-cardinality URL), method, status, total duration, attempt count, retry reason/delay, SDK version, error class, and request ID. For WebSockets record state transitions, handshake status, close code, reconnect attempt/delay, and connection duration.
- Never log request/response bodies or `Authorization`, `X-Session-Token`, access/refresh tokens, passwords, Steam/custom tokens, emails, storage values, or WebSocket payloads. Header capture must use an explicit safe allowlist. Emit one normal completion record per logical call; keep individual retry attempts at debug/trace level.

## WebSocket client (`/v1/ws`)

- Derive `wss://…/v1/ws` from the HTTPS base URL, use the same auth headers, verify TLS, and bound the opening handshake. Browser JavaScript cannot set these arbitrary headers through `new WebSocket(url, protocols)`; do **not** leak tokens into the URL. A browser SDK requires a server-supported secure ticket/cookie/subprotocol design first.
- Keep one continuous read loop so the library processes control frames and answers ggscale's server ping (every 30 seconds). Serialize writes, use bounded queues/backpressure, cap message size (the server accepts at most 1 MiB inbound), validate UTF-8/JSON envelopes, isolate callback failures, and safely ignore/log unknown event types.
- Reconnect after abnormal/network closure, `1001`, `1006`, `1011`-`1013`, or retryable handshake status. Do not reconnect after caller shutdown/`1000`, auth/policy/protocol failure, or terminal `4xx`. Delay the first retry randomly by **0–5 seconds**, then use capped full-jitter exponential backoff; honor handshake `Retry-After`, cap attempts by a caller-controlled deadline, and reset backoff only after a stable connection.
- Delivery is best effort, not exactly once. After every reconnect, re-read authoritative REST state (pending `/v1/invite`, active `/v1/matchmaker/tickets/{id}`, and relevant friends/presence data); tolerate duplicate or out-of-order notifications. On shutdown, send a normal Close frame, stop producers, drain or cancel bounded work, and release the socket.
- OpenAPI describes only the `101` upgrade, not realtime messages. Maintain an AsyncAPI or shared JSON Schema for the `{type,payload}` envelope, `presence`, `game_invite`, and `matchmaker_matched` payloads, close codes, compatibility rules, and recovery semantics; generate WebSocket models from that contract.

## Minimum conformance tests

Run each SDK against the local server on Linux and use fake clocks/transports for deterministic tests: all success codes, problem errors, both `Retry-After` formats, safe versus unsafe retries, deadline/cancellation during backoff, concurrent token refresh, pagination, ETag/preconditions, body limits, redaction, dropped WebSocket handshakes/connections, ping/pong, bounded queues, reconnect/resync, graceful close, and unknown fields/events.

Sources: [HTTP idempotency](https://www.rfc-editor.org/rfc/rfc9110.html#section-9.2.2), [`Retry-After`](https://www.rfc-editor.org/rfc/rfc9110.html#section-10.2.3), [`429`](https://www.rfc-editor.org/rfc/rfc6585.html#section-4), [timeouts/retries/jitter](https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/), [Problem Details](https://www.rfc-editor.org/rfc/rfc9457.html), [WebSocket protocol](https://www.rfc-editor.org/rfc/rfc6455.html), [OpenTelemetry HTTP conventions](https://opentelemetry.io/docs/specs/semconv/http/http-spans/), [OpenAPI 3.1](https://spec.openapis.org/oas/v3.1.0.html), [AsyncAPI WebSocket binding](https://www.asyncapi.com/docs/reference/bindings/websockets), [browser WebSocket API](https://websockets.spec.whatwg.org/), and [bearer-token URI risks](https://www.rfc-editor.org/rfc/rfc6750.html#section-2.3).
