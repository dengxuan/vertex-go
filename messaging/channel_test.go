// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

package messaging

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/dengxuan/vertex-go/transport"
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

// TestClose_DrainsInFlightResponses verifies that Close waits for in-flight
// dispatchRequest goroutines to finish flushing their responses before
// cancelling lifetimeCtx. Regression guard for the bug where a handler's
// transport.Send used the cancelled lifetimeCtx and dropped the response —
// the remote caller would see PeerDisconnectedError instead of the real reply.
func TestClose_DrainsInFlightResponses(t *testing.T) {
	a, b := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", b)

	// Slow handler — gives the test room to call Close() while a dispatch
	// is mid-flight (handler still running, response not yet sent).
	handlerRunning := make(chan struct{}, 1)
	if err := HandleRequest[*wrapperspb.StringValue, *wrapperspb.StringValue](cb,
		func(ctx context.Context, req *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
			select {
			case handlerRunning <- struct{}{}:
			default:
			}
			time.Sleep(200 * time.Millisecond)
			return &wrapperspb.StringValue{Value: "echo: " + req.Value}, nil
		}); err != nil {
		t.Fatalf("HandleRequest: %v", err)
	}

	invokeDone := make(chan error, 1)
	invokeResp := make(chan string, 1)
	go func() {
		ctx := context.Background()
		resp := &wrapperspb.StringValue{}
		err := ca.Invoke(ctx, "", &wrapperspb.StringValue{Value: "hi"}, resp, 5*time.Second)
		if err == nil {
			invokeResp <- resp.Value
		}
		invokeDone <- err
	}()

	// Wait until the handler has started (so Close actually has an in-flight
	// dispatch to drain).
	select {
	case <-handlerRunning:
	case <-time.After(3 * time.Second):
		t.Fatal("handler never started")
	}

	// Close mid-dispatch. Without the drain fix, the dispatcher's Send
	// would observe a cancelled lifetimeCtx and drop the response.
	closeStart := time.Now()
	cb.Close()
	closeElapsed := time.Since(closeStart)
	// Close should have waited roughly the remainder of the handler's sleep.
	if closeElapsed < 100*time.Millisecond {
		t.Errorf("Close returned too fast (%s) — drain did not wait for in-flight dispatch", closeElapsed)
	}

	err := <-invokeDone
	if err != nil {
		t.Fatalf("expected Invoke to succeed after Close drain; got %v", err)
	}
	resp := <-invokeResp
	if resp != "echo: hi" {
		t.Errorf("want %q, got %q", "echo: hi", resp)
	}
}

// TestClose_DrainTimeout verifies Close does not hang forever if a handler
// blocks past the drain deadline. It should log a warn and return within
// a bounded window so graceful-shutdown paths can make progress.
func TestClose_DrainTimeout(t *testing.T) {
	a, b := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	// Short drain so the test finishes quickly.
	cb := NewChannel("bob", b, WithCloseDrainTimeout(100*time.Millisecond))

	handlerRunning := make(chan struct{}, 1)
	if err := HandleRequest[*wrapperspb.StringValue, *wrapperspb.StringValue](cb,
		func(ctx context.Context, req *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
			select {
			case handlerRunning <- struct{}{}:
			default:
			}
			// Block far longer than the drain — Close must give up eventually.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Second):
				return &wrapperspb.StringValue{Value: "late"}, nil
			}
		}); err != nil {
		t.Fatalf("HandleRequest: %v", err)
	}

	go func() {
		ctx := context.Background()
		resp := &wrapperspb.StringValue{}
		_ = ca.Invoke(ctx, "", &wrapperspb.StringValue{Value: "x"}, resp, 2*time.Second)
	}()

	select {
	case <-handlerRunning:
	case <-time.After(3 * time.Second):
		t.Fatal("handler never started")
	}

	closeStart := time.Now()
	cb.Close()
	closeElapsed := time.Since(closeStart)

	// Should unblock shortly after the 100ms drain timeout — allow some slack
	// but fail if it hung for seconds.
	if closeElapsed > 2*time.Second {
		t.Errorf("Close hung past drain timeout: elapsed=%s (drain=100ms)", closeElapsed)
	}
}

// TestWithConnectionChangeListener_ReceivesEvents asserts the listener fires
// on every transport ConnectionEvent, in order, without racing the channel's
// own disconnect-handling.
func TestWithConnectionChangeListener_ReceivesEvents(t *testing.T) {
	a, _ := inMemTransportPair()

	var mu sync.Mutex
	var events []transport.ConnectionEvent
	ch := NewChannel("alice", a,
		WithConnectionChangeListener(func(e transport.ConnectionEvent) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		}))
	defer ch.Close()

	// Drive a couple of events onto the transport's Connections channel.
	a.conns <- transport.ConnectionEvent{Peer: "peer1", State: transport.Connected}
	a.conns <- transport.ConnectionEvent{Peer: "peer1", State: transport.Disconnected}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	if events[0].State != transport.Connected {
		t.Errorf("event 0: want Connected, got %v", events[0].State)
	}
	if events[1].State != transport.Disconnected {
		t.Errorf("event 1: want Disconnected, got %v", events[1].State)
	}
}

// TestSubscribeAll_ReceivesAllTopics 验证 wildcard subscriber 拿到所有 topic
// 的 envelope（不需要预注册 type / topic）。
func TestSubscribeAll_ReceivesAllTopics(t *testing.T) {
	a, b := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", b)
	defer cb.Close()

	received := make(chan Envelope, 4)
	cancel := SubscribeAll(cb, func(ctx context.Context, env Envelope) error {
		received <- env
		return nil
	})
	defer cancel()

	ctx, cancelCtx := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelCtx()

	if err := ca.Publish(ctx, "", &wrapperspb.StringValue{Value: "s1"}); err != nil {
		t.Fatalf("Publish StringValue: %v", err)
	}
	if err := ca.Publish(ctx, "", &wrapperspb.Int32Value{Value: 42}); err != nil {
		t.Fatalf("Publish Int32Value: %v", err)
	}

	got := make([]string, 0, 2)
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case env := <-received:
			got = append(got, env.Topic)
		case <-deadline:
			t.Fatalf("timeout: only got %d envelopes (%v)", len(got), got)
		}
	}
	wantTopics := map[string]bool{
		"google.protobuf.StringValue": true,
		"google.protobuf.Int32Value":  true,
	}
	for _, topic := range got {
		if !wantTopics[topic] {
			t.Errorf("unexpected topic %q (got: %v)", topic, got)
		}
	}
}

// TestSubscribeAll_PreservesCrossTopicOrder 这是 SubscribeAll 存在的根本原因 ——
// 跨 topic 顺序保留。Subscribe[T] 把不同 type 路由到独立 inbox+worker，跨 type
// race；SubscribeAll 单 inbox+worker，wire 顺序 = handler 调用顺序。
//
// 测试方法：交替发 100 条 String/Int 事件，校验 wildcard handler 收到的顺序
// 完全等同 publish 顺序。
func TestSubscribeAll_PreservesCrossTopicOrder(t *testing.T) {
	const N = 100
	a, b := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", b)
	defer cb.Close()

	received := make(chan string, N) // "s:0" / "i:1" / "s:2" / ...
	cancel := SubscribeAll(cb, func(ctx context.Context, env Envelope) error {
		// Encode topic-shorthand + payload as a single string for ordering check.
		// Don't unmarshal — we only care about sequence here.
		shorthand := "?"
		if strings.HasSuffix(env.Topic, ".StringValue") {
			shorthand = "s"
		} else if strings.HasSuffix(env.Topic, ".Int32Value") {
			shorthand = "i"
		}
		received <- shorthand
		return nil
	})
	defer cancel()

	ctx, cancelCtx := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCtx()

	want := make([]string, 0, N)
	for i := 0; i < N; i++ {
		if i%2 == 0 {
			if err := ca.Publish(ctx, "", &wrapperspb.StringValue{Value: ""}); err != nil {
				t.Fatalf("Publish String %d: %v", i, err)
			}
			want = append(want, "s")
		} else {
			if err := ca.Publish(ctx, "", &wrapperspb.Int32Value{Value: int32(i)}); err != nil {
				t.Fatalf("Publish Int %d: %v", i, err)
			}
			want = append(want, "i")
		}
	}

	got := make([]string, 0, N)
	deadline := time.After(5 * time.Second)
	for len(got) < N {
		select {
		case s := <-received:
			got = append(got, s)
		case <-deadline:
			t.Fatalf("timeout: only got %d/%d (%v)", len(got), N, got)
		}
	}

	// 最关键的断言：order 完全等于 publish 顺序。
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order broken at index %d: want %q, got %q (full got: %v)", i, want[i], got[i], got)
		}
	}
}

// TestSubscribeAll_CoexistsWithTypedSubscribe 验证 wildcard 和 typed sub 同时存在
// 互不影响 —— wildcard 拿全集，typed 拿自己 topic 的。
func TestSubscribeAll_CoexistsWithTypedSubscribe(t *testing.T) {
	a, b := inMemTransportPair()
	ca := NewChannel("alice", a)
	defer ca.Close()
	cb := NewChannel("bob", b)
	defer cb.Close()

	wildSeen := make(chan string, 2)
	cancelWild := SubscribeAll(cb, func(ctx context.Context, env Envelope) error {
		wildSeen <- env.Topic
		return nil
	})
	defer cancelWild()

	typedSeen := make(chan string, 1)
	cancelTyped, err := Subscribe[*wrapperspb.StringValue](cb, func(ctx context.Context, v *wrapperspb.StringValue) error {
		typedSeen <- v.Value
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancelTyped()

	ctx, cancelCtx := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelCtx()

	if err := ca.Publish(ctx, "", &wrapperspb.StringValue{Value: "hello"}); err != nil {
		t.Fatalf("Publish String: %v", err)
	}
	if err := ca.Publish(ctx, "", &wrapperspb.Int32Value{Value: 7}); err != nil {
		t.Fatalf("Publish Int: %v", err)
	}

	// wildcard 应该收到 2 条
	gotWild := []string{}
	deadline := time.After(2 * time.Second)
	for len(gotWild) < 2 {
		select {
		case t := <-wildSeen:
			gotWild = append(gotWild, t)
		case <-deadline:
			t.Fatalf("wildcard: only got %d/2 (%v)", len(gotWild), gotWild)
		}
	}

	// typed 只应该收到 String 那一条
	select {
	case v := <-typedSeen:
		if v != "hello" {
			t.Errorf("typed: want \"hello\", got %q", v)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("typed: did not receive StringValue within 500ms")
	}

	// typed 不应该再收到 Int32 那条
	select {
	case v := <-typedSeen:
		t.Errorf("typed: leaked Int32 as %q", v)
	case <-time.After(200 * time.Millisecond):
		// good
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
