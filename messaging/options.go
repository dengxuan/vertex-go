// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

package messaging

import (
	"context"
	"log/slog"
	"time"
)

// Option configures a Channel at construction time. Variadic functional-options
// pattern keeps NewChannel backwards compatible while leaving room to tune
// timeouts and buffer sizes for specific deployments.
type Option func(*channelOptions)

type channelOptions struct {
	invokeDefaultTimeout time.Duration
	errorResponseTimeout time.Duration
	subscriberInboxSize  int
	closeDrainTimeout    time.Duration
	logger               *slog.Logger
}

func defaultChannelOptions() channelOptions {
	return channelOptions{
		invokeDefaultTimeout: 30 * time.Second,
		errorResponseTimeout: 5 * time.Second,
		// 256 is a reasonable per-subscription queue depth for production
		// (the original MVP default of 32 was too tight under bursty load).
		// Events still drop on overflow — see WithSubscriberInboxSize docs.
		subscriberInboxSize: 256,
		// Matches transport/grpc's drainTimeout. Long enough for a slow
		// handler to flush its response, short enough that graceful
		// shutdown doesn't hang a misbehaving deployment.
		closeDrainTimeout: 5 * time.Second,
		logger:            discardLogger(),
	}
}

// WithInvokeDefaultTimeout sets the timeout Invoke uses when the caller passes
// 0. Default: 30s. Individual Invoke calls can still pass a tighter timeout.
func WithInvokeDefaultTimeout(d time.Duration) Option {
	return func(o *channelOptions) { o.invokeDefaultTimeout = d }
}

// WithErrorResponseTimeout bounds the best-effort Send that returns an error
// envelope to the caller when a handler fails or no handler is registered.
// Default: 5s. A failed error-response Send is silently dropped (invariant #2:
// a single reply failing to send must NOT be treated as a disconnect).
func WithErrorResponseTimeout(d time.Duration) Option {
	return func(o *channelOptions) { o.errorResponseTimeout = d }
}

// WithSubscriberInboxSize sets the per-subscriber event-queue depth. Each
// Subscribe-registered handler gets its own buffered channel of this size,
// drained by a dedicated worker goroutine. When the queue is full, newer
// events are DROPPED for that subscriber (not blocked) so one slow handler
// cannot stall the receive loop or other subscribers.
//
// Default: 256. Increase for bursty workloads; decrease for memory-tight
// deployments with low event rates. Monitor drops via [Channel.Stats].
func WithSubscriberInboxSize(n int) Option {
	return func(o *channelOptions) {
		if n > 0 {
			o.subscriberInboxSize = n
		}
	}
}

// WithCloseDrainTimeout bounds how long Close waits for in-flight
// dispatchRequest goroutines to finish flushing their responses before
// cancelling the channel's lifetime context.
//
// Default: 5s. Set to 0 (or negative) to keep the default — passing an
// explicit zero does not skip the drain. Increase only if handlers
// legitimately need more time to complete (e.g. slow downstream DB writes
// that still must flush on shutdown).
//
// When the drain times out, Vertex logs a Warn and proceeds; any handler
// whose response hasn't reached the transport yet will see its ctx
// cancelled and the remote caller will eventually hit its own timeout.
func WithCloseDrainTimeout(d time.Duration) Option {
	return func(o *channelOptions) {
		if d > 0 {
			o.closeDrainTimeout = d
		}
	}
}

// WithLogger wires a structured logger for the channel. Vertex logs at Warn
// level on back-pressure drops (subscriber inbox full), handler panics
// (recovered), and best-effort Send failures from internal paths (e.g. error
// responses). At Info level on channel start. No high-frequency per-message
// logging — your log pipeline stays quiet under normal load.
//
// If not set, Vertex uses a discard handler (zero output).
//
// Bridge example for zerolog / zap: wrap the target logger in a custom
// slog.Handler. Most production Go logging libraries provide slog bridges.
func WithLogger(l *slog.Logger) Option {
	return func(o *channelOptions) {
		if l != nil {
			o.logger = l
		}
	}
}

// discardLogger returns a logger that produces no output. Used as default when
// the user does not pass WithLogger. Equivalent to slog.DiscardHandler
// (stdlib in Go 1.24+) but defined locally for broader Go version support.
func discardLogger() *slog.Logger {
	return slog.New(discardHandler{})
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool   { return false }
func (discardHandler) Handle(context.Context, slog.Record) error  { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h discardHandler) WithGroup(string) slog.Handler            { return h }
