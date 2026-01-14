package mexc

import (
	"fmt"

	"google.golang.org/protobuf/proto"
)

// DecodeProtobuf is a helper function to decode protobuf messages
// This is a placeholder for when protobuf messages are used in MEXC WS v3
func DecodeProtobuf(data []byte, msg proto.Message) error {
	if err := proto.Unmarshal(data, msg); err != nil {
		return fmt.Errorf("failed to unmarshal protobuf: %w", err)
	}
	return nil
}

// EncodeProtobuf is a helper function to encode protobuf messages
func EncodeProtobuf(msg proto.Message) ([]byte, error) {
	data, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal protobuf: %w", err)
	}
	return data, nil
}
