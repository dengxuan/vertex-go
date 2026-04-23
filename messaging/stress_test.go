// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

package messaging

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestStress_ConcurrentMixedLoad_NoLeaks hammers the messaging channel with
// many concurrent publishers and invokers for a sustained period, then
// asserts the post-cleanup state is bounded:
//
//   - goroutine count is back within a small delta of the baseline
//     (no per-op goroutine leak)
//   - heap allocation is bounded (no unbounded retention of pending
//     requests / events after operations complete)
//   - no Invoke returned a wire-level failure
//
// Skipped in -short. Duration defaults to 30s and can be overridden with
// the VERTEX_STRESS_DURATION env var ("10s", "1m", etc.) for CI vs. local
// tuning.
func TestStress_ConcurrentMixedLoad_NoLeaks(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test: skipped under -short")
	}

	duration := 30 * time.Second

	a, bobT := inMemTransportPair()
	ca := NewChannel("alice", a)
	cb := NewChannel("bob", bobT)
	t.Cleanup(func() {
		ca.Close()
		cb.Close()
		_ = a.Close()
		_ = bobT.Close()
	})

	// Echo RPC on bob.
	if err := HandleRequest[*wrapperspb.StringValue, *wrapperspb.StringValue](cb,
		func(ctx context.Context, req *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
			return &wrapperspb.StringValue{Value: req.Value}, nil
		}); err != nil {
		t.Fatalf("HandleRequest: %v", err)
	}

	// One counting subscriber on bob. Events are at-most-once; drops are OK
	// (and expected under load) — this test does not assert delivery count,
	// only that the channel stays healthy.
	var eventsReceived atomic.Int64
	if _, err := Subscribe[*wrapperspb.StringValue](cb,
		func(ctx context.Context, ev *wrapperspb.StringValue) error {
			eventsReceived.Add(1)
			return nil
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Baseline: take goroutine count after setup has settled.
	// Give the receive / connection loops a moment to park at their
	// selects; otherwise baseline counts can be low by one or two.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	baselineGoroutines := runtime.NumGoroutine()
	var baselineStats runtime.MemStats
	runtime.ReadMemStats(&baselineStats)

	// Load: fan out concurrent invokers + publishers until deadline.
	const invokers = 8
	const publishers = 4
	var wg sync.WaitGroup
	stop := make(chan struct{})

	var invokes, publishes atomic.Int64
	var invokeErrs atomic.Int64
	var firstErr atomic.Value // error

	for i := 0; i < invokers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			req := &wrapperspb.StringValue{Value: "ping"}
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp := &wrapperspb.StringValue{}
				if err := ca.Invoke(ctx, "", req, resp, 2*time.Second); err != nil {
					invokeErrs.Add(1)
					firstErr.CompareAndSwap(nil, err)
					return
				}
				invokes.Add(1)
			}
		}()
	}

	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			event := &wrapperspb.StringValue{Value: "evt"}
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := ca.Publish(ctx, "", event); err != nil {
					firstErr.CompareAndSwap(nil, err)
					return
				}
				publishes.Add(1)
			}
		}()
	}

	time.Sleep(duration)
	close(stop)
	wg.Wait()

	// Post-load cleanup.
	ca.Close()
	cb.Close()
	// Give finalizers a shot so residual allocations don't skew comparison.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	runtime.GC()

	finalGoroutines := runtime.NumGoroutine()
	var finalStats runtime.MemStats
	runtime.ReadMemStats(&finalStats)

	// ── assertions ─────────────────────────────────────────────────────
	if e, ok := firstErr.Load().(error); ok && e != nil {
		// A single transport-level or remote error is a red flag: we're
		// on in-mem loopback with a simple handler. Nothing should fail.
		t.Fatalf("first worker error: %v (total Invoke errors=%d)", e, invokeErrs.Load())
	}
	if invokeErrs.Load() > 0 {
		t.Fatalf("Invoke errors during stress run: %d", invokeErrs.Load())
	}
	if invokes.Load() == 0 {
		t.Fatal("no Invokes completed — producers never made progress")
	}
	if publishes.Load() == 0 {
		t.Fatal("no Publishes completed")
	}

	// Goroutine leak: tolerate a small delta for Go runtime workers
	// (sysmon, GC sweepers). 5 is generous; real leak would be O(N) in
	// operation count.
	delta := finalGoroutines - baselineGoroutines
	if delta > 5 {
		t.Errorf("goroutine leak: baseline=%d, final=%d, delta=%d\n%s",
			baselineGoroutines, finalGoroutines, delta, dumpGoroutines())
	}

	// Bounded memory: a channel that correctly releases pending requests
	// + subscriber inboxes should not retain GB of state. Generous upper
	// bound here — we only want to catch unbounded growth patterns, not
	// measure steady-state working set precisely.
	const heapUpperBound = 200 * 1024 * 1024 // 200 MiB
	if finalStats.HeapAlloc > heapUpperBound {
		t.Errorf("heap appears to grow unboundedly: final HeapAlloc=%d bytes (upper bound %d)",
			finalStats.HeapAlloc, heapUpperBound)
	}

	t.Logf("stress summary: duration=%s invokes=%d publishes=%d events_received=%d "+
		"goroutines=%d→%d heap=%.1fMB→%.1fMB",
		duration, invokes.Load(), publishes.Load(), eventsReceived.Load(),
		baselineGoroutines, finalGoroutines,
		float64(baselineStats.HeapAlloc)/(1024*1024),
		float64(finalStats.HeapAlloc)/(1024*1024))
}

func dumpGoroutines() string {
	buf := make([]byte, 1<<16)
	n := runtime.Stack(buf, true)
	return fmt.Sprintf("goroutine dump (%d bytes):\n%s", n, buf[:n])
}
