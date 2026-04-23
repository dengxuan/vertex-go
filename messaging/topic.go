// Licensed to the Gordon under one or more agreements.
// Gordon licenses this file to you under the MIT license.

package messaging

import (
	"google.golang.org/protobuf/proto"
)

// TopicFor returns the on-wire topic string for a Protobuf-generated message.
// The convention matches .NET's MessageTopic.For<T>() for IMessage types:
// both emit the proto FullName (e.g. "feivoo.gaming.v1.CreateRoom"), so
// peers talking across language implementations automatically align.
func TopicFor(m proto.Message) string {
	return string(m.ProtoReflect().Descriptor().FullName())
}
