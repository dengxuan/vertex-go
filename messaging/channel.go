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
	"sync/atomic"
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

// Channel is a messaging wrapper around a [transport.Transport]. It supports
// both client-side flows (Publish, Invoke) and server-side dispatch (handlers
// registered via [Subscribe] and [HandleRequest]). Safe for concurrent use.
//
// Construct with [NewChannel]. Cancel with Close.
type Channel struct {
	name      string
	transport transport.Transport

	opts channelOptions

	mu           sync.Mutex // guards pending + subscribers + wildcardSubs + handlers + closed
	pending      map[string]*pendingRequest
	subscribers  map[string][]*subscription
	wildcardSubs []*wildcardSubscription
	handlers     map[string]requestHandlerFn
	closed       bool

	// statsMu guards the stats maps. Separate from mu so Stats() callers
	// don't contend with the hot dispatch path.
	statsMu       sync.RWMutex
	eventsDropped map[string]uint64

	// lifetimeCtx is cancelled by Close. Every dispatched event / request
	// handler and every channel-initiated Send derives its ctx from here,
	// so shutdown is cooperative without spawning a watcher goroutine per
	// dispatch.
	lifetimeCtx    context.Context
	lifetimeCancel context.CancelFunc

	// inFlightRequests counts dispatchRequest goroutines that are still
	// running. Close waits on this (bounded by closeDrainTimeout) before
	// cancelling lifetimeCtx, so a handler that returned a response but
	// hasn't yet flushed its transport.Send does not lose the response on
	// graceful shutdown.
	inFlightRequests sync.WaitGroup

	nextSubID atomic.Uint64
}

// subscription is one Subscribe-registered handler. Each subscription owns
// a private buffered inbox and a dedicated worker goroutine. Events fan out
// to all subscriptions for a topic; one slow subscriber only backpressures
// (and eventually drops into) its own inbox — other subscribers keep flowing
// and the receive loop never blocks.
type subscription struct {
	id    uint64
	topic string
	fn    subscriberFn
	inbox chan []byte
	// dropped is a monotonic counter of events rejected because inbox was full.
	// Exposed per-topic via Channel.Stats().
	dropped atomic.Uint64
	// cancelled is closed by the unsubscribe func to tell the worker to exit.
	cancelled chan struct{}
}

// ChannelStats is a snapshot of observable runtime state, safe for monitoring
// loops. Vertex does not reset counters — all fields are monotonic since
// channel construction. To compute a rate, sample twice and diff.
type ChannelStats struct {
	// EventsDropped is the total count of events NOT delivered because a
	// subscriber's inbox was full, aggregated per topic across all
	// subscriptions for that topic.
	//
	// A non-zero (and growing) value means at least one subscriber for that
	// topic cannot keep up with the event rate. Remedies: bigger inbox
	// (WithSubscriberInboxSize), a faster handler, or — if the loss is
	// not tolerable — switch that flow from Publish to Invoke so the sender
	// learns of failure.
	EventsDropped map[string]uint64
}

// subscriberFn is the internal form of an event subscriber. The outer generic
// [Subscribe] helper wraps a typed handler into this uniform signature so the
// receiveLoop dispatcher doesn't have to know the concrete type.
type subscriberFn func(ctx context.Context, payload []byte) error

// wildcardSubscription is a "subscribe to all topics" registration created by
// [SubscribeAll]. Unlike topic-typed [subscription] (one inbox per topic per
// sub), a wildcard sub gets every event delivered to the channel regardless
// of topic — and crucially, in **wire receive order across topics**, because
// the receiveLoop is single-goroutine and fan-out into the wildcard sub's
// inbox preserves enqueue order.
//
// Use case: consumer needs cross-topic ordering for the same logical entity
// (e.g. lottery issue lifecycle: opening → stopping → drawing → finished →
// next opening). With per-type [Subscribe], each type has its own inbox +
// worker → fan-out racing → cross-type order lost. SubscribeAll funnels
// everything through one inbox + one worker → wire order preserved.
type wildcardSubscription struct {
	id    uint64
	fn    wildcardSubscriberFn
	inbox chan Envelope
	// dropped is a monotonic counter of events rejected because inbox was full.
	// Aggregated under the "*" key in [ChannelStats.EventsDropped].
	dropped   atomic.Uint64
	cancelled chan struct{}
}

// wildcardSubscriberFn is the internal form of a wildcard subscriber.
type wildcardSubscriberFn func(ctx context.Context, env Envelope) error

// WildcardTopic is the [ChannelStats.EventsDropped] map key under which
// wildcard subscriber drops are aggregated (drops aren't per-topic since
// wildcard subs receive all topics).
const WildcardTopic = "*"

// requestHandlerFn is the internal form of an RPC handler. Returns either a
// marshalled response or an error (which gets turned into an error envelope).
type requestHandlerFn func(ctx context.Context, payload []byte) ([]byte, error)

type pendingRequest struct {
	resp chan responseBytes
}

type responseBytes struct {
	payload []byte
	isError bool
	err     error
}

// NewChannel wires a Channel to a transport. It spawns a goroutine that
// consumes transport.Receive() and dispatches responses / events / requests.
// Cancel by calling Close.
func NewChannel(name string, t transport.Transport, opts ...Option) *Channel {
	co := defaultChannelOptions()
	for _, o := range opts {
		o(&co)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := &Channel{
		name:           name,
		transport:      t,
		opts:           co,
		pending:        make(map[string]*pendingRequest),
		subscribers:    make(map[string][]*subscription),
		handlers:       make(map[string]requestHandlerFn),
		eventsDropped:  make(map[string]uint64),
		lifetimeCtx:    ctx,
		lifetimeCancel: cancel,
	}
	co.logger.Info("vertex messaging channel started",
		"channel", name,
		"transport", t.Name())
	go ch.receiveLoop()
	go ch.connectionLoop()
	return ch
}

// Stats returns a snapshot of monitoring counters. Safe to call from any
// goroutine; cheap (O(topics) map copy). Call at a steady cadence and diff
// to get rates.
func (c *Channel) Stats() ChannelStats {
	c.statsMu.RLock()
	dropped := make(map[string]uint64, len(c.eventsDropped))
	for k, v := range c.eventsDropped {
		dropped[k] = v
	}
	c.statsMu.RUnlock()
	return ChannelStats{EventsDropped: dropped}
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
		timeout = c.opts.invokeDefaultTimeout
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
	case <-c.lifetimeCtx.Done():
		return errors.New("vertex messaging: channel closed")
	}
}

// Close terminates the Channel. Pending invokes return an error. Underlying
// transport is NOT closed here — callers own that lifetime.
//
// Graceful behavior: Close refuses to spawn new dispatchRequest goroutines
// (c.closed gates the receiveLoop) and then waits up to closeDrainTimeout
// for goroutines already in flight to finish. Only after the drain window
// does it cancel lifetimeCtx. This prevents a common class of lost responses
// on graceful shutdown: a handler returns its response, but the dispatcher's
// transport.Send (which uses lifetimeCtx) gets its ctx cancelled before the
// bytes hit the wire — the remote caller then sees PeerDisconnectedError
// instead of the real response.
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

	// Drain in-flight dispatchRequest goroutines. Bounded: a pathological
	// handler blocking past closeDrainTimeout doesn't block shutdown forever
	// (we'll cancel lifetimeCtx anyway and let the remote time out).
	drainDone := make(chan struct{})
	go func() {
		c.inFlightRequests.Wait()
		close(drainDone)
	}()
	drainTimer := time.NewTimer(c.opts.closeDrainTimeout)
	defer drainTimer.Stop()
	select {
	case <-drainDone:
	case <-drainTimer.C:
		c.opts.logger.Warn("vertex messaging: close drain timeout elapsed; in-flight handlers may have dropped responses",
			"channel", c.name,
			"timeout", c.opts.closeDrainTimeout)
	}

	// Cancelling lifetimeCtx wakes receiveLoop / connectionLoop / every
	// in-flight dispatched handler that is honoring this ctx.
	c.lifetimeCancel()
}

func (c *Channel) receiveLoop() {
	recv := c.transport.Receive()
	for {
		select {
		case msg, ok := <-recv:
			if !ok {
				return // transport closed
			}
			env, err := Decode(msg.Frames)
			if err != nil {
				// Malformed — drop (invariant #1: the receive loop never dies on a bad message).
				continue
			}
			switch env.Kind {
			case KindResponse:
				c.dispatchResponse(env)
			case KindEvent:
				c.dispatchEvent(env)
			case KindRequest:
				// Request handling runs in its own goroutine — invariant #1 forbids
				// the receive loop from waiting on user code. Close drains these
				// goroutines so a handler's response gets a chance to flush before
				// lifetimeCtx is cancelled.
				c.mu.Lock()
				if c.closed {
					c.mu.Unlock()
					continue
				}
				c.inFlightRequests.Add(1)
				c.mu.Unlock()
				go func(env Envelope, from string) {
					defer c.inFlightRequests.Done()
					c.dispatchRequest(env, from)
				}(env, msg.From)
			}
		case <-c.lifetimeCtx.Done():
			return
		}
	}
}

func (c *Channel) connectionLoop() {
	conns := c.transport.Connections()
	for {
		select {
		case evt, ok := <-conns:
			if !ok {
				return // transport closed
			}
			if evt.State == transport.Disconnected {
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
			// Forward to the user listener AFTER internal handling so listeners
			// observe state transitions without racing the disconnect-path
			// cleanup. Best-effort: a listener that panics gets swallowed so
			// the connection loop stays alive.
			if c.opts.onConnectionChange != nil {
				func() {
					defer func() {
						if r := recover(); r != nil {
							c.opts.logger.Warn("vertex messaging: connection listener panicked",
								"channel", c.name,
								"panic", fmt.Sprintf("%v", r))
						}
					}()
					c.opts.onConnectionChange(evt)
				}()
			}
		case <-c.lifetimeCtx.Done():
			return
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

func (c *Channel) dispatchEvent(env Envelope) {
	c.mu.Lock()
	subs := append([]*subscription(nil), c.subscribers[env.Topic]...)
	wildSubs := append([]*wildcardSubscription(nil), c.wildcardSubs...)
	c.mu.Unlock()

	if len(subs) == 0 && len(wildSubs) == 0 {
		// No subscribers (typed or wildcard) → silently drop. Matches .NET semantics.
		return
	}

	// Fan out to topic-typed subscribers (existing behavior). Each gets the
	// raw payload bytes. Drop-on-full backpressure — one slow subscriber
	// cannot stall others or the receive loop (invariant #1).
	for _, sub := range subs {
		select {
		case sub.inbox <- env.Payload:
		default:
			// Dropped: this subscriber's inbox is full. Record it so ops
			// monitoring Channel.Stats() can notice, and log at Warn so
			// live diagnostics surface slow handlers in realtime.
			sub.dropped.Add(1)
			c.statsMu.Lock()
			c.eventsDropped[env.Topic]++
			c.statsMu.Unlock()
			c.opts.logger.Warn("vertex messaging: event dropped (subscriber inbox full)",
				"channel", c.name,
				"topic", env.Topic,
				"subscription_id", sub.id,
				"inbox_cap", cap(sub.inbox),
				"subscription_dropped_total", sub.dropped.Load())
		}
	}

	// Fan out to wildcard subscribers (SubscribeAll). They get the full
	// envelope including Topic — the consumer's responsibility to demux.
	// Same drop-on-full policy as typed subs; drops aggregated under
	// [WildcardTopic] key in stats.
	for _, ws := range wildSubs {
		select {
		case ws.inbox <- env:
		default:
			ws.dropped.Add(1)
			c.statsMu.Lock()
			c.eventsDropped[WildcardTopic]++
			c.statsMu.Unlock()
			c.opts.logger.Warn("vertex messaging: event dropped (wildcard subscriber inbox full)",
				"channel", c.name,
				"topic", env.Topic,
				"subscription_id", ws.id,
				"inbox_cap", cap(ws.inbox),
				"subscription_dropped_total", ws.dropped.Load())
		}
	}
}

func (c *Channel) dispatchRequest(env Envelope, from string) {
	c.mu.Lock()
	handler := c.handlers[env.Topic]
	c.mu.Unlock()

	if handler == nil {
		c.sendErrorResponse(env.Topic, env.RequestID, from,
			fmt.Sprintf("no RPC handler registered for %q on channel %q", env.Topic, c.name))
		return
	}

	ctx := c.lifetimeCtx
	respBytes, err := c.invokeHandlerSafely(ctx, handler, env.Payload)
	if err != nil {
		c.sendErrorResponse(env.Topic, env.RequestID, from, err.Error())
		return
	}

	resp := Envelope{
		Topic:     env.Topic,
		Kind:      KindResponse,
		RequestID: env.RequestID,
		Payload:   respBytes,
	}
	if err := c.transport.Send(ctx, from, resp.Encode()); err != nil {
		// Invariant #2: a single reply failing to send must NOT be treated as
		// a disconnect. Just drop.
		return
	}
}

// invokeHandlerSafely converts handler panics into errors so the dispatch loop
// stays alive.
func (c *Channel) invokeHandlerSafely(ctx context.Context, h requestHandlerFn, payload []byte) (resp []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			c.opts.logger.Error("vertex messaging: RPC handler panicked",
				"channel", c.name,
				"panic", fmt.Sprintf("%v", r))
			err = fmt.Errorf("vertex messaging: handler panicked: %v", r)
			resp = nil
		}
	}()
	return h(ctx, payload)
}

func (c *Channel) sendErrorResponse(topic, requestID, target, message string) {
	resp := Envelope{
		Topic:     ErrorTopicPrefix + topic,
		Kind:      KindResponse,
		RequestID: requestID,
		Payload:   []byte(message),
	}
	// Best-effort — use background ctx so ongoing per-call ctxs don't kill
	// this. Timeout is WithErrorResponseTimeout (default 5s).
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.errorResponseTimeout)
	defer cancel()
	if err := c.transport.Send(ctx, target, resp.Encode()); err != nil {
		// Invariant #2: a failed error-reply is still not a disconnect.
		// Log so ops sees RPC-level problems (handler path may be pathological).
		c.opts.logger.Warn("vertex messaging: error response Send failed (dropped)",
			"channel", c.name,
			"topic", topic,
			"request_id", requestID,
			"err", err.Error())
	}
}

// registerSubscriber is the internal registration used by the generic Subscribe.
// Each call spins up a dedicated worker goroutine; the worker exits when the
// subscription is cancelled OR the channel's lifetimeCtx is cancelled.
func (c *Channel) registerSubscriber(topic string, fn subscriberFn) func() {
	sub := &subscription{
		id:        c.nextSubID.Add(1),
		topic:     topic,
		fn:        fn,
		inbox:     make(chan []byte, c.opts.subscriberInboxSize),
		cancelled: make(chan struct{}),
	}

	go c.runSubscriberWorker(sub)

	c.mu.Lock()
	c.subscribers[topic] = append(c.subscribers[topic], sub)
	c.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			list := c.subscribers[topic]
			for i, s := range list {
				if s.id == sub.id {
					c.subscribers[topic] = append(list[:i], list[i+1:]...)
					break
				}
			}
			c.mu.Unlock()
			close(sub.cancelled)
		})
	}
}

// registerWildcardSubscriber is the internal registration used by [SubscribeAll].
// Each call spins up a dedicated worker goroutine + private inbox, like
// [registerSubscriber], but receives every event regardless of topic — and
// hence preserves cross-topic order = wire receive order.
func (c *Channel) registerWildcardSubscriber(fn wildcardSubscriberFn) func() {
	ws := &wildcardSubscription{
		id:        c.nextSubID.Add(1),
		fn:        fn,
		inbox:     make(chan Envelope, c.opts.subscriberInboxSize),
		cancelled: make(chan struct{}),
	}

	go c.runWildcardWorker(ws)

	c.mu.Lock()
	c.wildcardSubs = append(c.wildcardSubs, ws)
	c.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			for i, s := range c.wildcardSubs {
				if s.id == ws.id {
					c.wildcardSubs = append(c.wildcardSubs[:i], c.wildcardSubs[i+1:]...)
					break
				}
			}
			c.mu.Unlock()
			close(ws.cancelled)
		})
	}
}

func (c *Channel) runWildcardWorker(ws *wildcardSubscription) {
	for {
		select {
		case env, ok := <-ws.inbox:
			if !ok {
				return
			}
			c.invokeWildcardSafely(ws.fn, env)
		case <-ws.cancelled:
			return
		case <-c.lifetimeCtx.Done():
			return
		}
	}
}

// invokeWildcardSafely is the wildcard counterpart of invokeSubscriberSafely:
// contain handler panics (invariant #1 corollary) and silently swallow user
// handler errors — symmetric with the typed-subscriber path.
func (c *Channel) invokeWildcardSafely(fn wildcardSubscriberFn, env Envelope) {
	defer func() { _ = recover() }()
	_ = fn(c.lifetimeCtx, env)
}

func (c *Channel) runSubscriberWorker(sub *subscription) {
	for {
		select {
		case payload, ok := <-sub.inbox:
			if !ok {
				return
			}
			c.invokeSubscriberSafely(sub.fn, payload)
		case <-sub.cancelled:
			return
		case <-c.lifetimeCtx.Done():
			return
		}
	}
}

func (c *Channel) invokeSubscriberSafely(fn subscriberFn, payload []byte) {
	defer func() { _ = recover() }() // contain handler panics (invariant #1 corollary)
	_ = fn(c.lifetimeCtx, payload)
}

// registerHandler is the internal registration used by the generic HandleRequest.
func (c *Channel) registerHandler(topic string, fn requestHandlerFn) {
	c.mu.Lock()
	c.handlers[topic] = fn
	c.mu.Unlock()
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
