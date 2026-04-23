// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

// Package grpc is the Vertex gRPC bidi transport — the Go mirror of
// .NET's Vertex.Transport.Grpc. See the transport contract:
// https://github.com/dengxuan/Vertex/blob/main/spec/transport-contract.md
package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	grpcpkg "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dengxuan/vertex-go/transport"
	pb "github.com/dengxuan/vertex-go/transport/grpc/protocol/v1"
)

// Option configures a Transport at Dial time.
type Option func(*options)

type options struct {
	name            string
	serverAddr      string
	transportCreds  credentials.TransportCredentials
	dialOpts        []grpcpkg.DialOption
	inboundBuffer   int
	connectionBuffer int
}

// WithName overrides the transport's logical name (defaults to the server address).
func WithName(name string) Option { return func(o *options) { o.name = name } }

// WithTLS uses TLS for the gRPC dial. Defaults to insecure.
func WithTLS(creds credentials.TransportCredentials) Option {
	return func(o *options) { o.transportCreds = creds }
}

// WithDialOption appends an arbitrary grpc.DialOption.
func WithDialOption(opt grpcpkg.DialOption) Option {
	return func(o *options) { o.dialOpts = append(o.dialOpts, opt) }
}

// WithInboundBuffer sets the capacity of the inbound Message channel (default 256).
// Too small → slow dispatcher blocks the read loop; too large → memory.
func WithInboundBuffer(n int) Option { return func(o *options) { o.inboundBuffer = n } }

// Transport is a client-side gRPC bidi transport. It dials one server and
// reads/writes a single stream of [pb.TransportFrame]s.
//
// It satisfies the four invariants from transport-contract.md:
//   - #1: readLoop only pushes frames into an inbound channel; no user handlers.
//   - #2: Send errors bubble up; the transport stays alive for other requests.
//   - #3: ctx is honored while acquiring writeSem; after that Send(stream) runs
//     without ctx.
//   - #4: only readLoop emits Disconnected.
type Transport struct {
	name       string
	serverAddr string

	conn   *grpcpkg.ClientConn
	client pb.BidiClient
	stream grpcpkg.BidiStreamingClient[pb.TransportFrame, pb.TransportFrame]

	// writeSem is a 1-slot semaphore used as a ctx-cancellable mutex guarding
	// stream.Send. Buffered channel = classic Go trick for cancellable locking.
	writeSem chan struct{}

	inbound     chan transport.Message
	connections chan transport.ConnectionEvent

	closeOnce sync.Once
	closed    chan struct{}
}

// Dial establishes a bidi stream to the Vertex gRPC server at serverAddr and
// starts the read loop. Returns after the stream is opened successfully.
func Dial(ctx context.Context, serverAddr string, opts ...Option) (*Transport, error) {
	cfg := options{
		name:             serverAddr,
		serverAddr:       serverAddr,
		transportCreds:   insecure.NewCredentials(),
		inboundBuffer:    256,
		connectionBuffer: 8,
	}
	for _, o := range opts {
		o(&cfg)
	}

	dialOpts := append([]grpcpkg.DialOption{
		grpcpkg.WithTransportCredentials(cfg.transportCreds),
	}, cfg.dialOpts...)

	conn, err := grpcpkg.NewClient(serverAddr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("vertex grpc: dial %s: %w", serverAddr, err)
	}

	client := pb.NewBidiClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("vertex grpc: open bidi stream: %w", err)
	}

	t := &Transport{
		name:        cfg.name,
		serverAddr:  cfg.serverAddr,
		conn:        conn,
		client:      client,
		stream:      stream,
		writeSem:    make(chan struct{}, 1),
		inbound:     make(chan transport.Message, cfg.inboundBuffer),
		connections: make(chan transport.ConnectionEvent, cfg.connectionBuffer),
		closed:      make(chan struct{}),
	}

	// Emit Connected before starting readLoop. Subscribers that drain
	// connections() synchronously from start will see it.
	t.connections <- transport.ConnectionEvent{Peer: cfg.serverAddr, State: transport.Connected}

	go t.readLoop()

	return t, nil
}

// Name reports the transport's logical name.
func (t *Transport) Name() string { return t.name }

// Receive returns the inbound Message channel.
func (t *Transport) Receive() <-chan transport.Message { return t.inbound }

// Connections returns the connection state channel.
func (t *Transport) Connections() <-chan transport.ConnectionEvent { return t.connections }

// Send transmits the envelope frames as a multi-frame message. See the Transport
// interface docs for the cancellation contract.
func (t *Transport) Send(ctx context.Context, target string, frames [][]byte) error {
	// Acquire the write lock with ctx cancellation (invariant #3's allowed pre-wire cancel).
	select {
	case t.writeSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closed:
		return errors.New("vertex grpc: transport closed")
	}
	defer func() { <-t.writeSem }()

	// Once the first byte is on the wire, ctx is intentionally NOT passed into
	// stream.Send — cancelling mid-stream would issue HTTP/2 RST_STREAM and
	// tear down every in-flight request sharing the bidi stream.
	for i, f := range frames {
		eom := i == len(frames)-1
		if err := t.stream.Send(&pb.TransportFrame{Payload: f, EndOfMessage: eom}); err != nil {
			return fmt.Errorf("vertex grpc: stream send frame %d/%d: %w", i+1, len(frames), err)
		}
	}
	return nil
}

// Close shuts down the bidi stream and the gRPC connection. Idempotent.
func (t *Transport) Close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.closed)
		// Best-effort half-close the stream so the server's recv loop exits.
		_ = t.stream.CloseSend()
		err = t.conn.Close()
	})
	return err
}

// readLoop is the SOLE source of truth for the peer connection's liveness
// (invariant #4). It pushes every fully-assembled envelope into t.inbound.
func (t *Transport) readLoop() {
	var acc [][]byte
	defer func() {
		// Emit a terminal Disconnected once, close the outputs.
		select {
		case t.connections <- transport.ConnectionEvent{Peer: t.serverAddr, State: transport.Disconnected}:
		default:
		}
		close(t.inbound)
		close(t.connections)
	}()

	for {
		frame, err := t.stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// Log-worthy, but we don't have a logger here; caller can observe via Disconnected.
			}
			return
		}
		acc = append(acc, frame.GetPayload())
		if frame.GetEndOfMessage() {
			msg := transport.Message{From: t.serverAddr, Frames: acc}
			select {
			case t.inbound <- msg:
			case <-t.closed:
				return
			}
			acc = nil
		}
	}
}

// Compile-time check.
var _ transport.Transport = (*Transport)(nil)
