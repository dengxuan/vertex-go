// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

package messaging

import (
	"context"
	"errors"
	"sync"

	"github.com/dengxuan/vertex-go/transport"
)

// inMemTransportPair returns two linked transport.Transport instances; anything
// Send'd from one appears on the other's Receive() channel. The pair is
// deterministic (no real I/O) and suitable for messaging-layer tests.
func inMemTransportPair() (a, b *inMemTransport) {
	aInbound := make(chan transport.Message, 16)
	bInbound := make(chan transport.Message, 16)
	aConn := make(chan transport.ConnectionEvent, 4)
	bConn := make(chan transport.ConnectionEvent, 4)

	a = &inMemTransport{name: "alice", inbound: aInbound, peerInbound: bInbound, conns: aConn, closed: make(chan struct{})}
	b = &inMemTransport{name: "bob", inbound: bInbound, peerInbound: aInbound, conns: bConn, closed: make(chan struct{})}
	return a, b
}

type inMemTransport struct {
	name        string
	inbound     chan transport.Message // channel WE read from
	peerInbound chan transport.Message // channel the PEER reads from (where our Sends land)
	conns       chan transport.ConnectionEvent

	closeOnce sync.Once
	closed    chan struct{}
}

func (t *inMemTransport) Name() string                             { return t.name }
func (t *inMemTransport) Receive() <-chan transport.Message        { return t.inbound }
func (t *inMemTransport) Connections() <-chan transport.ConnectionEvent { return t.conns }

func (t *inMemTransport) Send(ctx context.Context, target string, frames [][]byte) error {
	// Defensive copy — tests should not see surprises if the caller mutates buffers.
	framesCopy := make([][]byte, len(frames))
	for i, f := range frames {
		b := make([]byte, len(f))
		copy(b, f)
		framesCopy[i] = b
	}
	msg := transport.Message{From: t.name, Frames: framesCopy}

	select {
	case <-t.closed:
		return errors.New("inMemTransport: closed")
	default:
	}

	select {
	case t.peerInbound <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closed:
		return errors.New("inMemTransport: closed")
	}
}

func (t *inMemTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		close(t.inbound)
		close(t.conns)
	})
	return nil
}
