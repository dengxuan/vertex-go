# vertex-go

> Go implementation of [Vertex](https://github.com/dengxuan/Vertex) — a lightweight, cross-language bidi messaging kernel.

[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## Install

```bash
go get github.com/dengxuan/vertex-go@v1.1.0
```

Go 1.22+. Cross-language peer: [vertex-dotnet](https://github.com/dengxuan/vertex-dotnet).

## What's in the box

| Package | Role |
|---|---|
| [`github.com/dengxuan/vertex-go/transport`](./transport) | `Transport` interface + 4 invariants (see [spec](https://github.com/dengxuan/Vertex/blob/main/spec/transport-contract.md)) |
| [`github.com/dengxuan/vertex-go/transport/grpc`](./transport/grpc) | gRPC bidi client: non-blocking Dial, exponential-backoff reconnect, graceful Close |
| [`github.com/dengxuan/vertex-go/messaging`](./messaging) | `Channel` with `Publish` / `Invoke` / `Subscribe[T]` / `HandleRequest[Req, Resp]` |
| [`github.com/dengxuan/vertex-go/transport/grpc/protocol/v1`](./transport/grpc/protocol/v1) | protoc-generated `TransportFrame` + `Bidi` stub |

Server-side transport (Go-as-gRPC-server receiving from any language peer) is **not** in this release — track it in issues if you need it.

## Quickstart

Client publishes a fire-and-forget event:

```go
import (
    "context"
    "github.com/dengxuan/vertex-go/messaging"
    vgrpc "github.com/dengxuan/vertex-go/transport/grpc"
    gamingv1 "myapp/gen/gaming/v1"  // your protoc-generated types
)

ctx := context.Background()

transport, err := vgrpc.Dial(ctx, "api.example.com:443",
    vgrpc.WithTLS(credentials.NewTLS(&tls.Config{})))
if err != nil { return err }
defer transport.Close()  // graceful: CloseSend flushes queued frames

channel := messaging.NewChannel("gaming", transport,
    messaging.WithLogger(slog.Default()),            // recommended in prod
    messaging.WithSubscriberInboxSize(1024),         // default 256
)
defer channel.Close()

// 1-way event
_ = channel.Publish(ctx, "", &gamingv1.GameStateChanged{RoomId: "lobby", Phase: 3})

// 2-way RPC (acknowledged)
resp := &gamingv1.RoomCreated{}
err = channel.Invoke(ctx, "", &gamingv1.CreateRoom{RoomName: "lobby"}, resp, 5*time.Second)
```

Server-side subscribe / handle (works any side that has an inbound stream):

```go
cancel, err := messaging.Subscribe[*gamingv1.GameStateChanged](channel,
    func(ctx context.Context, evt *gamingv1.GameStateChanged) error {
        log.Info("state changed", "room", evt.RoomId, "phase", evt.Phase)
        return nil
    })
defer cancel()

_ = messaging.HandleRequest[*gamingv1.CreateRoom, *gamingv1.RoomCreated](channel,
    func(ctx context.Context, req *gamingv1.CreateRoom) (*gamingv1.RoomCreated, error) {
        return &gamingv1.RoomCreated{RoomId: newRoomID(req.RoomName)}, nil
    })
```

Cross-language topic alignment is automatic — `messaging.TopicFor(&gamingv1.CreateRoom{})` returns `"gaming.v1.CreateRoom"`, the same string `MessageTopic.For<CreateRoom>()` produces in .NET.

---

## Delivery semantics — READ BEFORE PRODUCTION

Vertex is **transport-layer messaging**, not a durable broker. Understanding what it guarantees is the difference between a working production deploy and a support nightmare.

### `Publish` = fire-and-forget, at-most-once

`channel.Publish(ctx, target, event)` sends an `EVENT` envelope over the current stream. **No acknowledgement, no retry, no persistence.** If any of these happens, the event is lost:

- the network drops the bytes mid-flight
- the server crashes between receive and dispatch
- the subscriber's inbox is full (configurable; see below)
- the subscriber handler panics (contained; counted; event not reprocessed)

Vertex does **not** try to mask this. `Publish` returns `nil` once frames are on the wire; it returns an error if the transport can't even accept them. It does not and cannot tell you whether the subscriber ran successfully.

**Use `Publish` for:** real-time notifications, cache invalidations, telemetry, anything the app can tolerate losing.

**Don't use `Publish` for:** orders, payments, audit log entries, state-mutation commands, anything with a business correctness requirement.

### `Invoke` = request/response, at-most-once with error signalling

`channel.Invoke(ctx, target, req, resp, timeout)` sends a `REQUEST` and waits for the matching `RESPONSE`. **If anything goes wrong the caller learns about it:**

| Failure mode | What `Invoke` returns |
|---|---|
| Transport fails to send | the Send error |
| Server has no handler for this type | `*RemoteError{Message: "no RPC handler registered…"}` |
| Server handler panics or returns error | `*RemoteError{Message: <error>}` |
| Connection drops before response arrives | `*PeerDisconnectedError` |
| Response doesn't arrive within `timeout` | `ErrTimeout` |
| Context cancelled / deadline exceeded | `ctx.Err()` |

The caller still has to decide what to do (retry, give up, fallback). Vertex only guarantees you **know** whether it worked.

**Use `Invoke` for:** anything with a business correctness requirement. Decide yourself about idempotency and retries.

### Decision table

| Your flow | Recommended |
|---|---|
| "The UI needs a live update" | `Publish` |
| "Tell the cache to evict this key" | `Publish` |
| "A customer paid $49" | `Invoke` (or broker + outbox) |
| "Provision a game room" | `Invoke` |
| "Emit an audit log entry" | `Invoke`, or broker + outbox if you need durability |
| "Broadcast a heartbeat every 5s" | `Publish` |

### Truly durable messaging (at-least-once, survive crashes)

Not in scope for Vertex. Use Kafka / RabbitMQ / NATS JetStream. You can wrap either in Vertex-compatible `Publish` / `Invoke` APIs on top if you want a uniform surface — that's an application-level pattern, not a library responsibility.

---

## Production checklist

Before you point real traffic at a Vertex-based client:

- [ ] **Every `Publish` / `Invoke` call uses a ctx with a deadline.** Don't pass `context.Background()` — a stuck server becomes a stuck caller. `context.WithTimeout(parent, 5*time.Second)` is a sane baseline.
- [ ] **`WithLogger` passes a real `*slog.Logger`.** Default is silent. You want Warn-level lines when drops or panics happen — that's the only way to notice the app degrading before users do.
- [ ] **Sample `channel.Stats()` at a fixed interval** (say 10s) and export `EventsDropped[topic]` to your metrics system. A non-zero rate means a subscriber is slower than the publisher — either widen the inbox, make the handler faster, or accept the loss with eyes open.
- [ ] **`WithSubscriberInboxSize` matches your burst profile.** Default is 256. If you produce 10k events/sec in a 500ms handler spike, 256 is 20× too small; bump to 8192 or more.
- [ ] **`Invoke` timeouts ≤ server handler's worst-case latency.** Match both sides; otherwise callers bail while the handler still completes silently.
- [ ] **Call `transport.Close()` on shutdown** (typically `defer`). Graceful close flushes in-flight sends; a hard kill drops the tail of a send batch.
- [ ] **Don't panic in subscribers / handlers.** Vertex recovers so the dispatch loop stays alive, but you lose observability of what went wrong. Return errors instead; they're logged.

---

## Reconnect behaviour

Client transport dials lazily. `Dial` returns immediately; the first `Publish` / `Invoke` / `WaitForConnected(ctx)` call blocks until a stream is available or ctx times out.

On disconnect (network blip, server restart), the transport emits `ConnectionEvent{State: Disconnected}` and retries with exponential backoff:

| Attempt | Backoff (with 20% jitter) |
|---|---|
| 1 | 1s |
| 2 | 2s |
| 3 | 4s |
| 4 | 8s |
| ... | ... capped at 30s |

During the disconnect window, `Publish` and `Invoke` **block up to the caller's ctx** waiting for the next stream — they don't fail fast unless ctx expires. Disable with `WithReconnect(vgrpc.ReconnectPolicy{Enabled: false})` for testing or single-shot tools.

In-flight `Invoke` calls when a disconnect fires receive `*PeerDisconnectedError`. Publish-er doesn't know about past events — they may or may not have landed.

---

## Observability — `Channel.Stats()`

```go
stats := channel.Stats()
for topic, dropped := range stats.EventsDropped {
    metrics.GaugeSet("vertex_events_dropped_total", float64(dropped), "topic", topic)
}
```

Counters are **monotonic since channel construction**. Compute rates by sampling twice and diffing. No reset API — create a new `Channel` if you need a fresh baseline.

---

## Spec & interop

- Wire format: [Vertex / spec / wire-format.md](https://github.com/dengxuan/Vertex/blob/main/spec/wire-format.md)
- Transport contract (4 invariants): [Vertex / spec / transport-contract.md](https://github.com/dengxuan/Vertex/blob/main/spec/transport-contract.md)
- Cross-language interop test: [Vertex / compat / hello](https://github.com/dengxuan/Vertex/tree/main/compat/hello)

## Contributing

See [`CONTRIBUTING.md`](./CONTRIBUTING.md). Wire-format changes go through the spec repo with a companion PR here.

## License

MIT — see [`LICENSE`](./LICENSE).
