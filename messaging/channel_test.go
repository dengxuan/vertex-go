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
