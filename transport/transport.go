// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

// Package transport is the byte-level transport abstraction. It does not know
// about message semantics or serialization — those live in the messaging layer.
//
// Every concrete Transport implementation MUST satisfy the four invariants
// documented at https://github.com/dengxuan/Vertex/blob/main/spec/transport-contract.md
package transport

import (
	"context"
	"io"
)

// Message is one multi-frame message handed up to the messaging layer.
type Message struct {
	// From identifies the sender. For a client-side transport with a single
	// server peer, this is the server's address; for multi-peer transports
	// (Router / gRPC server), it's the caller-provided peer id.
	From string

	// Frames is the raw byte frames of the envelope. At the wire layer these
	// are opaque — the messaging layer interprets them as the 4-frame envelope
	// from /spec/wire-format.md § 2.
	Frames [][]byte
}

// ConnectionState indicates a peer connection transition.
type ConnectionState int

const (
	// Connected means the transport has successfully established / re-established
	// a usable connection to the peer.
	Connected ConnectionState = iota
	// Disconnected means the transport observed a connection failure on the
	// read path. Only the read loop is allowed to emit Disconnected — invariant #4.
	Disconnected
)

// ConnectionEvent is emitted on connection state transitions.
type ConnectionEvent struct {
	Peer  string
	State ConnectionState
}

// Transport is the contract every wire adapter implements.
type Transport interface {
	io.Closer

	// Name returns the logical name of this transport instance.
	Name() string

	// Send transmits a multi-frame envelope to target as an atomic unit.
	//
	// Cancellation contract (invariant #3):
	//   ctx is honored ONLY while waiting for resources (write lock, connection,
	//   backpressure). Once any byte of the envelope has started hitting the
	//   wire, the implementation MUST ignore ctx cancellation and either flush
	//   all frames or surface a transport-level error. Honoring ctx mid-write
	//   on a gRPC bidi stream issues RST_STREAM which tears down every in-flight
	//   request sharing the HTTP/2 stream.
	Send(ctx context.Context, target string, frames [][]byte) error

	// Receive returns a channel of inbound messages. Single-consumer: the
	// messaging layer owns exactly one goroutine that ranges over it.
	// The channel closes when the transport is Close'd or the peer disconnects
	// terminally.
	Receive() <-chan Message

	// Connections returns a channel of connection state transitions.
	// The channel closes when the transport is Close'd.
	Connections() <-chan ConnectionEvent
}
