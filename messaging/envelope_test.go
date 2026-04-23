// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

package messaging

import (
	"bytes"
	"testing"
)

func TestEnvelope_EncodeDecode_Roundtrip(t *testing.T) {
	cases := []Envelope{
		{Topic: "gaming.v1.Hello", Kind: KindEvent, RequestID: "", Payload: []byte{1, 2, 3}},
		{Topic: "gaming.v1.CreateRoom", Kind: KindRequest, RequestID: "req-1", Payload: []byte("abc")},
		{Topic: "gaming.v1.CreateRoom", Kind: KindResponse, RequestID: "req-1", Payload: []byte{}},
		{Topic: "!gaming.v1.CreateRoom", Kind: KindResponse, RequestID: "req-2", Payload: []byte("boom")},
	}
	for _, c := range cases {
		t.Run(c.Topic, func(t *testing.T) {
			frames := c.Encode()
			if len(frames) != 4 {
				t.Fatalf("expected 4 frames, got %d", len(frames))
			}
			got, err := Decode(frames)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Topic != c.Topic || got.Kind != c.Kind || got.RequestID != c.RequestID {
				t.Errorf("topic/kind/reqid mismatch: got=%+v want=%+v", got, c)
			}
			if !bytes.Equal(got.Payload, c.Payload) {
				t.Errorf("payload mismatch: got=%x want=%x", got.Payload, c.Payload)
			}
		})
	}
}

func TestEnvelope_Decode_TooFewFrames(t *testing.T) {
	_, err := Decode([][]byte{[]byte("only-topic")})
	if err == nil {
		t.Fatal("expected error for 1 frame, got nil")
	}
}

func TestEnvelope_Decode_KindFrameWrongSize(t *testing.T) {
	_, err := Decode([][]byte{
		[]byte("topic"),
		{0, 0}, // two bytes, should be one
		[]byte("req"),
		[]byte("payload"),
	})
	if err == nil {
		t.Fatal("expected error for oversized kind frame, got nil")
	}
}
