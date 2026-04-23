# vertex-go

> Go implementation of [Vertex](https://github.com/dengxuan/Vertex) — a lightweight, cross-language bidi messaging kernel.

[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## Status: 🚧 planning

Code-level implementation starts alongside `vertex-dotnet` reaching a working baseline ported from Skywalker.

## Install (planned)

```bash
go get github.com/dengxuan/vertex-go/messaging
go get github.com/dengxuan/vertex-go/transport/grpc
```

Go version: **1.22+**.

## Package layout (planned)

```
github.com/dengxuan/vertex-go/
├── messaging/                 ← IMessageBus, IRpcClient, IRpcHandler equivalents
├── transport/
│   ├── grpc/                  ← gRPC bidi transport; Protobuf enforced
│   └── netmq/                 ← ZeroMQ transport (likely via pebbe/zmq4 or gomq)
├── serialization/
│   ├── protobuf/              ← required by transport/grpc
│   └── msgpack/               ← default for transport/netmq
└── protocol/v1/               ← protoc-generated code from the shared protos
```

## Getting started (planned, not functional yet)

Define business messages in `.proto`:

```proto
// protos/gaming.proto
syntax = "proto3";
package gaming.v1;
option go_package = "myapp/gen/gaming/v1;gamingv1";

message CreateRoom  { string room_name = 1; }
message RoomCreated { string room_id   = 1; }
```

Generate:

```bash
protoc --go_out=gen --go_opt=paths=source_relative protos/gaming.proto
```

Client:

```go
package main

import (
    "context"

    "github.com/dengxuan/vertex-go/messaging"
    "github.com/dengxuan/vertex-go/transport/grpc"
    gamingv1 "myapp/gen/gaming/v1"
)

func main() {
    ctx := context.Background()

    transport, err := grpc.Dial("https://api.example.com",
        grpc.WithBearerToken("my-api-key"))
    if err != nil {
        panic(err)
    }
    defer transport.Close()

    channel := messaging.NewChannel(transport,
        messaging.RegisterEvent[*gamingv1.GameStateChanged](),
        messaging.RegisterRequest[*gamingv1.CreateRoom, *gamingv1.RoomCreated](),
    )

    resp, err := messaging.Invoke[*gamingv1.CreateRoom, *gamingv1.RoomCreated](
        ctx, channel, &gamingv1.CreateRoom{RoomName: "lobby"})
    if err != nil {
        panic(err)
    }
    println(resp.RoomId)
}
```

Server-side handler:

```go
channel.RegisterHandler(
    messaging.Handler[*gamingv1.CreateRoom, *gamingv1.RoomCreated](
        func(ctx context.Context, req *gamingv1.CreateRoom) (*gamingv1.RoomCreated, error) {
            id := uuid.NewString()
            return &gamingv1.RoomCreated{RoomId: id}, nil
        }),
)
```

## Building (once code lands)

```bash
go mod download
go vet ./...
go test -race -cover ./...
```

## Spec

The authoritative wire format and transport contract live in the [Vertex spec repo](https://github.com/dengxuan/Vertex). **Any wire-spec change lands there first**, with a companion PR here.

Key documents:

- [Wire format](https://github.com/dengxuan/Vertex/blob/main/spec/wire-format.md)
- [Transport contract (4 invariants)](https://github.com/dengxuan/Vertex/blob/main/spec/transport-contract.md) — every Go transport impl MUST satisfy these

## Interop with .NET

.NET peer: [vertex-dotnet](https://github.com/dengxuan/vertex-dotnet). Cross-language E2E tests live in the spec repo under [`/compat/`](https://github.com/dengxuan/Vertex/tree/main/compat), exercising every (lang-A sender, lang-B receiver) pair.

## Contributing

See [`CONTRIBUTING.md`](./CONTRIBUTING.md).

## License

MIT — see [`LICENSE`](./LICENSE).
