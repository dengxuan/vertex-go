// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

package messaging

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Benchmarks cover the messaging layer's hot paths using the in-memory
// transport pair, so transport throughput does not dominate the numbers.
// Run with:
//   go test -bench=. -benchmem -benchtime=2s ./messaging/
//
// These are throughput / latency baselines, not pass/fail gates. They
// establish a reference point we can diff against when landing future
// changes.
//
// Note on Publish semantics: Publish is at-most-once. Under high throughput
// the default 256-slot subscriber inbox may drop events — that is documented
// behavior. The Publish benchmarks therefore measure only the producer path
// (serialize + envelope + transport.Send + (in-mem) peerInbound enqueue) and
// do NOT wait for every event to reach the handler; waiting would produce
// deadlocks whenever N exceeds the inbox size. Invoke benchmarks DO wait
// per-request because Invoke semantics require it.

// BenchmarkPublish_ProducerPath measures the time to serialize + enqueue
// one event. No subscriber is attached on the peer side; bytes land in the
// peer's inbound channel and are discarded by the test tear-down. The
// baseline here answers: "how fast can one goroutine emit events?"
func BenchmarkPublish_ProducerPath(b *testing.B) {
	a, bobT := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", bobT)
	defer cb.Close()

	event := &wrapperspb.StringValue{Value: "hello"}
	ctx := context.Background()

	// Drain bob's inbound in the background; the in-mem transport has a
	// bounded buffer (16) and would otherwise backpressure the Send call
	// and pollute the producer numbers.
	drainDone := make(chan struct{})
	go func() {
		for range bobT.Receive() {
			// discard
		}
		close(drainDone)
	}()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ca.Publish(ctx, "", event); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()

	// Trigger drainer shutdown via Close order: transport close → channel
	// shuts down receive loop → inbound chan closes → drain goroutine exits.
	_ = bobT.Close()
	<-drainDone
}

// BenchmarkPublish_EndToEnd covers serialize → transport → receiveLoop →
// dispatchEvent → subscriber worker → handler invocation, with a subscriber
// inbox sized large enough to absorb every event in the run. This is the
// "everything works" number — how fast can one Publish be observed by one
// Subscribe. Allocations here include the subscriber-side unmarshal.
func BenchmarkPublish_EndToEnd(b *testing.B) {
	a, bobT := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	// Size the subscriber inbox generously so high-N runs don't drop events
	// and leave the test waiting forever for a count it can't reach.
	cb := NewChannel("bob", bobT, WithSubscriberInboxSize(max(b.N, 256)))
	defer cb.Close()

	var received atomic.Int64
	if _, err := Subscribe[*wrapperspb.StringValue](cb, func(ctx context.Context, ev *wrapperspb.StringValue) error {
		received.Add(1)
		return nil
	}); err != nil {
		b.Fatalf("Subscribe: %v", err)
	}

	event := &wrapperspb.StringValue{Value: "hello"}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ca.Publish(ctx, "", event); err != nil {
			b.Fatal(err)
		}
	}

	// Wait for the subscriber worker to drain. Use a polling wait with a
	// generous deadline — the bench is done once all events are counted.
	deadline := time.Now().Add(30 * time.Second)
	for received.Load() < int64(b.N) {
		if time.Now().After(deadline) {
			b.Fatalf("only received %d/%d events within 30s", received.Load(), b.N)
		}
		time.Sleep(time.Millisecond)
	}
	b.StopTimer()
}

// BenchmarkInvoke_Sequential measures one full Invoke round-trip under zero
// concurrency: serialize request → send → handler runs → response →
// unmarshal. Each Invoke waits for its response before starting the next,
// so this is the p50-ish latency number.
func BenchmarkInvoke_Sequential(b *testing.B) {
	a, bobT := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", bobT)
	defer cb.Close()

	if err := HandleRequest[*wrapperspb.StringValue, *wrapperspb.StringValue](cb,
		func(ctx context.Context, req *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
			return &wrapperspb.StringValue{Value: req.Value}, nil
		}); err != nil {
		b.Fatalf("HandleRequest: %v", err)
	}

	ctx := context.Background()
	req := &wrapperspb.StringValue{Value: "hi"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := &wrapperspb.StringValue{}
		if err := ca.Invoke(ctx, "", req, resp, 2*time.Second); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInvoke_Parallel measures throughput with many in-flight requests
// sharing one channel. Each parallel goroutine tightly loops Invoke.
// Exercises the concurrent pending-request map and the handler's per-request
// goroutine fan-out.
func BenchmarkInvoke_Parallel(b *testing.B) {
	a, bobT := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", bobT)
	defer cb.Close()

	if err := HandleRequest[*wrapperspb.StringValue, *wrapperspb.StringValue](cb,
		func(ctx context.Context, req *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
			return &wrapperspb.StringValue{Value: req.Value}, nil
		}); err != nil {
		b.Fatalf("HandleRequest: %v", err)
	}

	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := &wrapperspb.StringValue{Value: "hi"}
		for pb.Next() {
			resp := &wrapperspb.StringValue{}
			if err := ca.Invoke(ctx, "", req, resp, 5*time.Second); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkPublish_FanoutSubscribers measures cost as subscriber count
// scales. One Publish, N subscribers each receive an unmarshaled copy.
// ns/op grows roughly linearly in subscriber count — fan-out is
// per-subscriber, not free.
func BenchmarkPublish_FanoutSubscribers(b *testing.B) {
	for _, nSub := range []int{1, 4, 16, 64} {
		b.Run(fmt.Sprintf("subs=%d", nSub), func(b *testing.B) {
			a, bobT := inMemTransportPair()
			ca := NewChannel("alice", a)
			defer ca.Close()
			cb := NewChannel("bob", bobT, WithSubscriberInboxSize(max(b.N, 256)))
			defer cb.Close()

			var received atomic.Int64
			for i := 0; i < nSub; i++ {
				if _, err := Subscribe[*wrapperspb.StringValue](cb, func(ctx context.Context, ev *wrapperspb.StringValue) error {
					received.Add(1)
					return nil
				}); err != nil {
					b.Fatalf("Subscribe: %v", err)
				}
			}

			event := &wrapperspb.StringValue{Value: "hello"}
			ctx := context.Background()
			want := int64(b.N) * int64(nSub)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := ca.Publish(ctx, "", event); err != nil {
					b.Fatal(err)
				}
			}

			deadline := time.Now().Add(30 * time.Second)
			for received.Load() < want {
				if time.Now().After(deadline) {
					b.Fatalf("only received %d/%d deliveries within 30s", received.Load(), want)
				}
				time.Sleep(time.Millisecond)
			}
			b.StopTimer()
		})
	}
}
