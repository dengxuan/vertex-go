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
