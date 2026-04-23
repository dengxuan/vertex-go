// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

package messaging

import "time"

// Option configures a Channel at construction time. Variadic functional-options
// pattern keeps NewChannel backwards compatible while leaving room to tune
// timeouts and buffer sizes for specific deployments.
type Option func(*channelOptions)

type channelOptions struct {
	invokeDefaultTimeout time.Duration
	errorResponseTimeout time.Duration
	subscriberInboxSize  int
}

func defaultChannelOptions() channelOptions {
	return channelOptions{
		invokeDefaultTimeout: 30 * time.Second,
		errorResponseTimeout: 5 * time.Second,
		subscriberInboxSize:  32,
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
// Default: 32. Increase for bursty workloads; decrease for memory-tight
// deployments with low event rates.
func WithSubscriberInboxSize(n int) Option {
	return func(o *channelOptions) {
		if n > 0 {
			o.subscriberInboxSize = n
		}
	}
}
