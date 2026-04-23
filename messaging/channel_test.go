// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Tests use well-known Protobuf wrapper types (wrapperspb.StringValue,
// wrapperspb.Int32Value) so we don't have to maintain a throwaway .proto.
// Their FullNames are "google.protobuf.StringValue" / "google.protobuf.Int32Value".

func TestPublish_DeliversToRemoteSubscriber(t *testing.T) {
	a, b := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", b)
	defer cb.Close()

	received := make(chan string, 1)
	cancel, err := Subscribe[*wrapperspb.StringValue](cb, func(ctx context.Context, v *wrapperspb.StringValue) error {
		received <- v.Value
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	ctx, cancelCtx := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelCtx()

	if err := ca.Publish(ctx, "", &wrapperspb.StringValue{Value: "hello"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-received:
		if got != "hello" {
			t.Errorf("want \"hello\", got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not fire within 2s")
	}
}

func TestPublish_UnregisteredEvent_Dropped(t *testing.T) {
	a, b := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", b)
	defer cb.Close() // bob has no subscriber

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Should not panic / error: an event with no subscriber is silently dropped.
	if err := ca.Publish(ctx, "", &wrapperspb.StringValue{Value: "nobody listens"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Give the dispatcher a moment; nothing to assert beyond "didn't crash".
	time.Sleep(100 * time.Millisecond)
}

func TestInvoke_RoundTripsSuccessResponse(t *testing.T) {
	a, b := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", b)
	defer cb.Close()

	err := HandleRequest[*wrapperspb.StringValue, *wrapperspb.StringValue](cb, func(ctx context.Context, req *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
		return &wrapperspb.StringValue{Value: "echo: " + req.Value}, nil
	})
	if err != nil {
		t.Fatalf("HandleRequest: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp := &wrapperspb.StringValue{}
	if err := ca.Invoke(ctx, "", &wrapperspb.StringValue{Value: "hi"}, resp, time.Second); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Value != "echo: hi" {
		t.Errorf("want \"echo: hi\", got %q", resp.Value)
	}
}

func TestInvoke_HandlerError_SurfacesAsRemoteError(t *testing.T) {
	a, b := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", b)
	defer cb.Close()

	if err := HandleRequest[*wrapperspb.StringValue, *wrapperspb.StringValue](cb, func(ctx context.Context, req *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
		return nil, errors.New("boom")
	}); err != nil {
		t.Fatalf("HandleRequest: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp := &wrapperspb.StringValue{}
	err := ca.Invoke(ctx, "", &wrapperspb.StringValue{Value: "ignored"}, resp, time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("expected RemoteError, got %T: %v", err, err)
	}
	if !strings.Contains(remote.Message, "boom") {
		t.Errorf("expected error message to contain \"boom\", got %q", remote.Message)
	}
}

func TestStats_TracksSubscriberInboxDrops(t *testing.T) {
	// Force a tiny inbox so we can saturate it deterministically.
	a, b := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", b, WithSubscriberInboxSize(1))
	defer cb.Close()

	// Slow subscriber: blocks inside the handler so its inbox fills fast.
	unblock := make(chan struct{})
	cancel, err := Subscribe[*wrapperspb.StringValue](cb, func(ctx context.Context, v *wrapperspb.StringValue) error {
		<-unblock // handler never returns until test releases it
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	defer close(unblock) // let the worker drain on teardown

	ctx, cancelCtx := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelCtx()

	// Publish enough events to overflow the 1-slot inbox. The first one gets
	// pulled into the worker (blocked in handler). The next fills the inbox.
	// Events 3+ are dropped.
	const N = 10
	for i := 0; i < N; i++ {
		if err := ca.Publish(ctx, "", &wrapperspb.StringValue{Value: "x"}); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	// Give the drop path a moment to observe.
	deadline := time.Now().Add(2 * time.Second)
	var got uint64
	topic := string((&wrapperspb.StringValue{}).ProtoReflect().Descriptor().FullName())
	for time.Now().Before(deadline) {
		got = cb.Stats().EventsDropped[topic]
		if got > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got == 0 {
		t.Fatalf("expected at least some drops, got 0; stats=%+v", cb.Stats())
	}
	t.Logf("dropped %d/%d events (expected at least 1)", got, N)
}

func TestInvoke_NoHandler_ReturnsRemoteError(t *testing.T) {
	a, b := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", b) // no handler registered
	defer cb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp := &wrapperspb.StringValue{}
	err := ca.Invoke(ctx, "", &wrapperspb.StringValue{Value: "x"}, resp, time.Second)
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("expected RemoteError, got %T: %v", err, err)
	}
	if !strings.Contains(remote.Message, "no RPC handler") {
		t.Errorf("unexpected remote message: %q", remote.Message)
	}
}
