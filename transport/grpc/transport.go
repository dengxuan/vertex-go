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
	"math"
	"math/rand/v2"
	"sync"
	"time"

	grpcpkg "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dengxuan/vertex-go/transport"
	pb "github.com/dengxuan/vertex-go/transport/grpc/protocol/v1"
)

// Option configures a Transport at Dial time.
type Option func(*options)

// ReconnectPolicy controls automatic reconnect after a dropped stream. The zero
// value is valid and gives Vertex's defaults (see DefaultReconnectPolicy).
type ReconnectPolicy struct {
	// Enabled toggles reconnection. When false, a dropped or never-established
	// stream terminates the runLoop — callers will observe a terminal
	// Disconnected event and subsequent Send calls return an error.
	Enabled bool
	// InitialBackoff is the wait before the first retry after a failure.
	InitialBackoff time.Duration
	// MaxBackoff caps the exponential growth.
	MaxBackoff time.Duration
	// Multiplier is the growth factor per failed attempt (e.g. 2.0 doubles each time).
	Multiplier float64
	// Jitter is added as a fraction of the current backoff, uniformly in
	// [-Jitter*b, +Jitter*b]. 0.0 disables jitter.
	Jitter float64
}

// DefaultReconnectPolicy returns Enabled=true with exponential backoff
// 1s → 30s and 20% jitter. Matches Vertex.Dotnet.Transport.Grpc defaults.
func DefaultReconnectPolicy() ReconnectPolicy {
	return ReconnectPolicy{
		Enabled:        true,
		InitialBackoff: time.Second,
		MaxBackoff:     30 * time.Second,
		Multiplier:     2.0,
		Jitter:         0.2,
	}
}

type options struct {
	name             string
	serverAddr       string
	transportCreds   credentials.TransportCredentials
	dialOpts         []grpcpkg.DialOption
	inboundBuffer    int
	connectionBuffer int
	reconnect        ReconnectPolicy
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
func WithInboundBuffer(n int) Option { return func(o *options) { o.inboundBuffer = n } }

// WithReconnect overrides the default reconnect policy. Pass
// ReconnectPolicy{Enabled: false} to disable automatic reconnection.
func WithReconnect(p ReconnectPolicy) Option { return func(o *options) { o.reconnect = p } }

// Transport is a client-side gRPC bidi transport. It owns a single gRPC
// ClientConn and runs a background goroutine that dials, reads, and reconnects.
//
// Invariants (see transport-contract.md):
//   - #1: readLoop pushes frames into an inbound channel; never invokes user handlers.
//   - #2: Send errors bubble up; the transport does not tear itself down.
//   - #3: Send's ctx is honored only while waiting for the write lock and the
//     current stream. Once a frame has started hitting the wire, ctx is dropped.
//   - #4: Only runLoop emits Disconnected; it is the sole observer of stream death.
type Transport struct {
	name       string
	serverAddr string
	conn       *grpcpkg.ClientConn
	reconnect  ReconnectPolicy

	// mu guards stream + connectedCh.
	mu sync.Mutex
	// stream is the current live bidi stream. nil when the runLoop is between
	// attempts (disconnected or dialing).
	stream grpcpkg.BidiStreamingClient[pb.TransportFrame, pb.TransportFrame]
	// connectedCh is closed when the stream becomes non-nil. Replaced with a
	// fresh open channel after each disconnect so subsequent waiters are
	// released by the next successful connect.
	connectedCh chan struct{}

	// writeSem serializes concurrent Send callers on the same stream; gRPC
	// does not allow concurrent writes on a single bidi stream. 1-slot
	// buffered channel = ctx-cancellable mutex.
	writeSem chan struct{}

	inbound     chan transport.Message
	connections chan transport.ConnectionEvent

	lifetimeCtx    context.Context
	lifetimeCancel context.CancelFunc
	doneLoop       chan struct{}

	closeOnce sync.Once
}

// Dial constructs a Transport. It does NOT block on the first connection —
// the background runLoop handles dial + read + reconnect. Send blocks until
// the first stream is up (or the caller's ctx expires); use WaitForConnected
// if you want to gate other work on connectivity.
//
// The only failure mode here is an invalid gRPC ClientConn config; network
// errors surface later through the runLoop's Disconnected events.
func Dial(ctx context.Context, serverAddr string, opts ...Option) (*Transport, error) {
	_ = ctx // Signature kept for symmetry; dial is non-blocking today.

	cfg := options{
		name:             serverAddr,
		serverAddr:       serverAddr,
		transportCreds:   insecure.NewCredentials(),
		inboundBuffer:    256,
		connectionBuffer: 8,
		reconnect:        DefaultReconnectPolicy(),
	}
	for _, o := range opts {
		o(&cfg)
	}

	dialOpts := append([]grpcpkg.DialOption{
		grpcpkg.WithTransportCredentials(cfg.transportCreds),
	}, cfg.dialOpts...)

	conn, err := grpcpkg.NewClient(serverAddr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("vertex grpc: create client for %s: %w", serverAddr, err)
	}

	lifetimeCtx, lifetimeCancel := context.WithCancel(context.Background())

	t := &Transport{
		name:           cfg.name,
		serverAddr:     cfg.serverAddr,
		conn:           conn,
		reconnect:      cfg.reconnect,
		connectedCh:    make(chan struct{}),
		writeSem:       make(chan struct{}, 1),
		inbound:        make(chan transport.Message, cfg.inboundBuffer),
		connections:    make(chan transport.ConnectionEvent, cfg.connectionBuffer),
		lifetimeCtx:    lifetimeCtx,
		lifetimeCancel: lifetimeCancel,
		doneLoop:       make(chan struct{}),
	}

	go t.runLoop()

	return t, nil
}

// Name reports the transport's logical name.
func (t *Transport) Name() string { return t.name }

// Receive returns the inbound Message channel.
func (t *Transport) Receive() <-chan transport.Message { return t.inbound }

// Connections returns the connection state channel.
func (t *Transport) Connections() <-chan transport.ConnectionEvent { return t.connections }

// WaitForConnected blocks until a stream becomes available or ctx is done.
// Useful in tests and eager-init code that wants to gate work on first connect.
func (t *Transport) WaitForConnected(ctx context.Context) error {
	_, err := t.waitForStream(ctx)
	return err
}

// Send transmits envelope frames as a multi-frame message.
func (t *Transport) Send(ctx context.Context, target string, frames [][]byte) error {
	// Acquire the write lock (invariant #3's allowed pre-wire cancel).
	select {
	case t.writeSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.lifetimeCtx.Done():
		return errors.New("vertex grpc: transport closed")
	}
	defer func() { <-t.writeSem }()

	// Wait for a live stream. If the runLoop is between attempts, this
	// blocks until the next Connected. ctx gates the wait itself, which is
	// still pre-wire.
	stream, err := t.waitForStream(ctx)
	if err != nil {
		return err
	}

	// From here on, ctx is intentionally NOT passed into stream.Send —
	// cancelling mid-stream would issue HTTP/2 RST_STREAM and tear down
	// every in-flight request sharing the bidi stream (invariant #3).
	for i, f := range frames {
		eom := i == len(frames)-1
		if err := stream.Send(&pb.TransportFrame{Payload: f, EndOfMessage: eom}); err != nil {
			return fmt.Errorf("vertex grpc: stream send frame %d/%d: %w", i+1, len(frames), err)
		}
	}
	return nil
}

// Close shuts down the runLoop, the bidi stream, and the gRPC connection.
// Idempotent. Safe to call from multiple goroutines.
func (t *Transport) Close() error {
	var err error
	t.closeOnce.Do(func() {
		t.lifetimeCancel()
		// Wait for runLoop to exit so we can close outputs exactly once.
		<-t.doneLoop
		err = t.conn.Close()
	})
	return err
}

// runLoop: dial → read → reconnect with backoff. SOLE source of Connected /
// Disconnected events (invariant #4). Lifetime ends when lifetimeCtx is cancelled.
func (t *Transport) runLoop() {
	defer func() {
		close(t.inbound)
		close(t.connections)
		close(t.doneLoop)
	}()

	attempt := 0
	for {
		if t.lifetimeCtx.Err() != nil {
			return
		}

		// Open a fresh bidi stream. grpc.NewClient was lazy; the actual
		// connect + HTTP/2 handshake happens on first RPC (= Connect here).
		client := pb.NewBidiClient(t.conn)
		stream, err := client.Connect(t.lifetimeCtx)
		if err != nil {
			if !t.sleepForBackoff(&attempt) {
				return
			}
			continue
		}

		// Publish the new stream and wake any Send waiters.
		t.setStream(stream)
		attempt = 0
		t.emitConnection(transport.Connected)

		// Drain inbound frames until the stream errors out.
		t.readLoop(stream)

		// Stream dead.
		t.setStream(nil)
		t.emitConnection(transport.Disconnected)

		if !t.reconnect.Enabled {
			return
		}

		if !t.sleepForBackoff(&attempt) {
			return
		}
	}
}

func (t *Transport) sleepForBackoff(attempt *int) bool {
	if !t.reconnect.Enabled {
		return false
	}
	*attempt++
	d := t.computeBackoff(*attempt)
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-t.lifetimeCtx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (t *Transport) computeBackoff(attempt int) time.Duration {
	p := t.reconnect
	if p.Multiplier <= 0 {
		p.Multiplier = 2.0
	}
	base := float64(p.InitialBackoff) * math.Pow(p.Multiplier, float64(attempt-1))
	if max := float64(p.MaxBackoff); max > 0 && base > max {
		base = max
	}
	if p.Jitter > 0 {
		jitter := base * math.Min(math.Max(p.Jitter, 0), 1)
		base += (rand.Float64()*2 - 1) * jitter
	}
	if base < 0 {
		base = 0
	}
	return time.Duration(base)
}

func (t *Transport) readLoop(stream grpcpkg.BidiStreamingClient[pb.TransportFrame, pb.TransportFrame]) {
	var acc [][]byte
	for {
		frame, err := stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// Stream-fatal; caller (runLoop) will emit Disconnected.
			}
			return
		}
		acc = append(acc, frame.GetPayload())
		if frame.GetEndOfMessage() {
			msg := transport.Message{From: t.serverAddr, Frames: acc}
			select {
			case t.inbound <- msg:
			case <-t.lifetimeCtx.Done():
				return
			}
			acc = nil
		}
	}
}

// setStream swaps the active stream under the lock. When s != nil, closes
// the connectedCh to wake waiters and creates a fresh one for the next cycle.
func (t *Transport) setStream(s grpcpkg.BidiStreamingClient[pb.TransportFrame, pb.TransportFrame]) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stream = s
	if s != nil {
		close(t.connectedCh)
		t.connectedCh = make(chan struct{})
	}
}

func (t *Transport) waitForStream(ctx context.Context) (grpcpkg.BidiStreamingClient[pb.TransportFrame, pb.TransportFrame], error) {
	for {
		t.mu.Lock()
		if t.stream != nil {
			s := t.stream
			t.mu.Unlock()
			return s, nil
		}
		ch := t.connectedCh
		t.mu.Unlock()

		select {
		case <-ch:
			// A stream was just published; loop back and pick it up.
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.lifetimeCtx.Done():
			return nil, errors.New("vertex grpc: transport closed")
		}
	}
}

func (t *Transport) emitConnection(state transport.ConnectionState) {
	select {
	case t.connections <- transport.ConnectionEvent{Peer: t.serverAddr, State: state}:
	case <-t.lifetimeCtx.Done():
	}
}

// Compile-time check.
var _ transport.Transport = (*Transport)(nil)
