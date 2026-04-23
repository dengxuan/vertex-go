// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

package messaging

import (
	"errors"
	"fmt"
)

// MessageKind enumerates the three envelope roles. Values on the wire MUST
// match /spec/wire-format.md § 2.2.
type MessageKind byte

const (
	// KindEvent is a fire-and-forget publish. RequestID is empty.
	KindEvent MessageKind = 0
	// KindRequest initiates an RPC. RequestID is a unique correlation id.
	KindRequest MessageKind = 1
	// KindResponse answers a request. RequestID matches the request. An error
	// response prefixes the topic with "!".
	KindResponse MessageKind = 2
)

// ErrorTopicPrefix marks a Response envelope as carrying an error (payload is
// UTF-8 error text, not a serialized response message). See § 2.4.
const ErrorTopicPrefix = "!"

// frameCount is the fixed 4-frame envelope size (§ 2).
const frameCount = 4

// Frame indices (§ 2, table).
const (
	frameTopic     = 0
	frameKind      = 1
	frameRequestID = 2
	framePayload   = 3
)

// Envelope is the decoded view of the 4-frame message the wire carries.
type Envelope struct {
	Topic     string
	Kind      MessageKind
	RequestID string
	Payload   []byte
}

// Encode builds the 4 wire frames. Byte order and contents match the spec.
func (e Envelope) Encode() [][]byte {
	return [][]byte{
		[]byte(e.Topic),
		{byte(e.Kind)},
		[]byte(e.RequestID),
		e.Payload,
	}
}

// Decode parses exactly 4 frames into an Envelope, or returns an error for a
// malformed frame set. Per § 2, fewer-than-4 frames is a spec violation and
// the receiver should drop the message.
func Decode(frames [][]byte) (Envelope, error) {
	if len(frames) < frameCount {
		return Envelope{}, fmt.Errorf("vertex messaging: envelope needs %d frames, got %d", frameCount, len(frames))
	}
	if len(frames[frameKind]) != 1 {
		return Envelope{}, errors.New("vertex messaging: kind frame must be exactly 1 byte")
	}
	return Envelope{
		Topic:     string(frames[frameTopic]),
		Kind:      MessageKind(frames[frameKind][0]),
		RequestID: string(frames[frameRequestID]),
		Payload:   frames[framePayload],
	}, nil
}
