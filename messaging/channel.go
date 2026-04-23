// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

// Package messaging is the client-side messaging layer on top of a Vertex
// [transport.Transport]. It provides:
//
//   - Publish: fire-and-forget events (no response expected)
//   - Invoke:  strongly-typed RPC request/response over the 4-frame envelope
//
// This MVP focuses on the client role. Server-side handler dispatch is deferred.
package messaging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/dengxuan/vertex-go/transport"
)

// ErrTimeout is returned by Invoke when the per-call timeout elapses before
// a response arrives. The pending request is cleaned up in the finalizer.
var ErrTimeout = errors.New("vertex messaging: rpc timed out")

// RemoteError wraps a server-reported error response (topic prefixed with "!").
type RemoteError struct {
	Topic   string
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("vertex messaging: remote error on %q: %s", e.Topic, e.Message)
}

// PeerDisconnectedError is returned from pending Invokes when the transport
// signals Disconnected before a response arrives.
type PeerDisconnectedError struct {
	Peer string
}

func (e *PeerDisconnectedError) Error() string {
	return fmt.Sprintf("vertex messaging: peer %s disconnected before response", e.Peer)
}

// Channel is a client-side messaging wrapper around a [transport.Transport].
// Construct with [NewChannel]. Safe for concurrent use.
type Channel struct {
	name      string
	transport transport.Transport

	mu      sync.Mutex // guards pending + closed
	pending map[string]*pendingRequest
	closed  bool

	done chan struct{}
}

type pendingRequest struct {
	resp chan responseBytes
}

type responseBytes struct {
	payload []byte
	isError bool
	err     error
}

// NewChannel wires a Channel to a transport. It spawns a goroutine that
// consumes transport.Receive() and dispatches responses. Cancel by calling Close.
func NewChannel(name string, t transport.Transport) *Channel {
	ch := &Channel{
		name:      name,
		transport: t,
		pending:   make(map[string]*pendingRequest),
		done:      make(chan struct{}),
	}
	go ch.receiveLoop()
	go ch.connectionLoop()
	return ch
}

// Publish sends a fire-and-forget event to the default peer. target may be empty
// for single-peer transports (like the gRPC client).
func (c *Channel) Publish(ctx context.Context, target string, event proto.Message) error {
	payload, err := proto.Marshal(event)
	if err != nil {
		return fmt.Errorf("vertex messaging: marshal event %T: %w", event, err)
	}
	env := Envelope{
		Topic:     TopicFor(event),
		Kind:      KindEvent,
		RequestID: "",
		Payload:   payload,
	}
	return c.transport.Send(ctx, target, env.Encode())
}

// Invoke sends a request and waits for the matching response. The caller
// supplies both request and a pre-allocated response target: Invoke marshals
// the request, sends it, and when the response arrives, unmarshals into response.
//
// The per-call timeout applies end-to-end (serialize + send + wait for reply).
// If ctx is already expired when Invoke is called, it returns ctx.Err().
func (c *Channel) Invoke(
	ctx context.Context,
	target string,
	request proto.Message,
	response proto.Message,
	timeout time.Duration,
) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	payload, err := proto.Marshal(request)
	if err != nil {
		return fmt.Errorf("vertex messaging: marshal request %T: %w", request, err)
	}

	requestID, err := newRequestID()
	if err != nil {
		return fmt.Errorf("vertex messaging: generate request id: %w", err)
	}

	pending := &pendingRequest{resp: make(chan responseBytes, 1)}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("vertex messaging: channel closed")
	}
	c.pending[requestID] = pending
	c.mu.Unlock()
	defer c.removePending(requestID)

	env := Envelope{
		Topic:     TopicFor(request),
		Kind:      KindRequest,
		RequestID: requestID,
		Payload:   payload,
	}
	if err := c.transport.Send(ctx, target, env.Encode()); err != nil {
		return fmt.Errorf("vertex messaging: send request: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case r := <-pending.resp:
		if r.err != nil {
			return r.err
		}
		if r.isError {
			return &RemoteError{Topic: env.Topic, Message: string(r.payload)}
		}
		if err := proto.Unmarshal(r.payload, response); err != nil {
			return fmt.Errorf("vertex messaging: unmarshal response %T: %w", response, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrTimeout
	case <-c.done:
		return errors.New("vertex messaging: channel closed")
	}
}

// Close terminates the Channel. Pending invokes return an error. Underlying
// transport is NOT closed here — callers own that lifetime.
func (c *Channel) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = map[string]*pendingRequest{}
	c.mu.Unlock()

	for _, p := range pending {
		select {
		case p.resp <- responseBytes{err: errors.New("vertex messaging: channel closed")}:
		default:
		}
	}
	close(c.done)
}

func (c *Channel) receiveLoop() {
	for msg := range c.transport.Receive() {
		env, err := Decode(msg.Frames)
		if err != nil {
			// Malformed — drop (invariant: loop must not die on a single bad message).
			continue
		}
		switch env.Kind {
		case KindResponse:
			c.dispatchResponse(env)
		case KindEvent, KindRequest:
			// Server-side dispatch not yet implemented in the Go MVP.
			// Silently ignore so a server pushing spontaneous events doesn't crash us.
		}
	}
}

func (c *Channel) connectionLoop() {
	for evt := range c.transport.Connections() {
		if evt.State != transport.Disconnected {
			continue
		}
		// Fail every in-flight invoke with a disconnected error.
		c.mu.Lock()
		pending := c.pending
		c.pending = map[string]*pendingRequest{}
		c.mu.Unlock()
		for _, p := range pending {
			select {
			case p.resp <- responseBytes{err: &PeerDisconnectedError{Peer: evt.Peer}}:
			default:
			}
		}
	}
}

func (c *Channel) dispatchResponse(env Envelope) {
	c.mu.Lock()
	pending, ok := c.pending[env.RequestID]
	if ok {
		delete(c.pending, env.RequestID)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	isError := len(env.Topic) > 0 && env.Topic[0] == ErrorTopicPrefix[0]
	select {
	case pending.resp <- responseBytes{payload: env.Payload, isError: isError}:
	default:
	}
}

func (c *Channel) removePending(requestID string) {
	c.mu.Lock()
	delete(c.pending, requestID)
	c.mu.Unlock()
}

func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
