// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

package messaging

import (
	"context"
	"fmt"
	"reflect"

	"google.golang.org/protobuf/proto"
)

// Subscribe registers an event handler for messages of type T. Multiple
// subscribers per type are permitted; each is invoked on its own goroutine.
//
// T must be a pointer to a Protobuf-generated message (e.g. *gaming.v1.HelloEvent).
// The topic is derived from the proto descriptor's FullName so cross-language
// peers align without coordination.
//
// Returns a cancel function that removes this subscription.
func Subscribe[T proto.Message](
	c *Channel,
	handler func(ctx context.Context, event T) error,
) (func(), error) {
	sample, rt, err := newProtoFactory[T]()
	if err != nil {
		return nil, err
	}
	topic := string(sample.ProtoReflect().Descriptor().FullName())

	wrapper := func(ctx context.Context, payload []byte) error {
		msg := reflect.New(rt).Interface().(proto.Message)
		if err := proto.Unmarshal(payload, msg); err != nil {
			return fmt.Errorf("vertex messaging: unmarshal event %s: %w", topic, err)
		}
		return handler(ctx, msg.(T))
	}

	return c.registerSubscriber(topic, wrapper), nil
}

// SubscribeAll registers a wildcard event handler that receives every event
// envelope published to the channel, regardless of topic — and crucially, in
// **wire receive order across topics**.
//
// Why this exists: the typed [Subscribe] creates one inbox + one worker per
// type T. If a consumer registers Subscribe for both T1 and T2, events of
// type T1 and T2 arrive in two independent goroutines with no cross-type
// ordering guarantee (each subscription's worker drains its own inbox
// independently). For consumers that need cross-type ordering on the same
// logical entity (e.g. a state machine where the producer publishes events
// of multiple types in a strict order), Subscribe[T] is unsuitable.
//
// SubscribeAll funnels every envelope through one inbox + one worker. Since
// the channel's receiveLoop is a single goroutine, enqueue order = wire
// receive order, so the handler observes events in exactly the order the
// transport delivered them.
//
// Trade-off vs Subscribe[T]: the consumer is responsible for demuxing by
// [Envelope.Topic] and unmarshalling the payload to the appropriate proto
// type. Vertex stays out of typed dispatch — that's a consumer-layer concern.
//
// Drop-on-full backpressure is identical to typed subs: a slow handler
// cannot stall the receive loop or other subscribers; drops are aggregated
// under the [WildcardTopic] key in [ChannelStats.EventsDropped].
//
// Returns a cancel function; calling it removes this wildcard subscription.
func SubscribeAll(c *Channel, handler func(ctx context.Context, env Envelope) error) func() {
	return c.registerWildcardSubscriber(handler)
}

// HandleRequest registers the single RPC handler for type Req → Resp on the
// channel. Only one handler per Req type; re-registering replaces the previous
// handler.
//
// Both Req and Resp must be pointers to Protobuf-generated messages.
func HandleRequest[Req proto.Message, Resp proto.Message](
	c *Channel,
	handler func(ctx context.Context, req Req) (Resp, error),
) error {
	reqSample, reqType, err := newProtoFactory[Req]()
	if err != nil {
		return err
	}
	topic := string(reqSample.ProtoReflect().Descriptor().FullName())

	wrapper := func(ctx context.Context, payload []byte) ([]byte, error) {
		req := reflect.New(reqType).Interface().(proto.Message)
		if err := proto.Unmarshal(payload, req); err != nil {
			return nil, fmt.Errorf("vertex messaging: unmarshal request %s: %w", topic, err)
		}
		resp, err := handler(ctx, req.(Req))
		if err != nil {
			return nil, err
		}
		return proto.Marshal(resp)
	}

	c.registerHandler(topic, wrapper)
	return nil
}

// newProtoFactory returns (sample proto.Message, element reflect.Type) for
// building fresh T instances via reflect.New(rt). T is expected to be a
// pointer-to-proto-struct (the idiomatic shape for protoc-gen-go output).
func newProtoFactory[T proto.Message]() (proto.Message, reflect.Type, error) {
	var zero T
	rt := reflect.TypeOf(zero)
	if rt == nil {
		return nil, nil, fmt.Errorf("vertex messaging: cannot determine type for generic T — pass a concrete pointer-to-proto-message type")
	}
	if rt.Kind() != reflect.Ptr {
		return nil, nil, fmt.Errorf("vertex messaging: generic T must be a pointer (protoc-gen-go emits pointer receivers); got %s", rt.Kind())
	}
	elem := rt.Elem()
	sample := reflect.New(elem).Interface().(proto.Message)
	return sample, elem, nil
}
