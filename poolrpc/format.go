package poolrpc

import (
	"fmt"

	"github.com/lightninglabs/protobuf-hex-display/jsonpb"
	"github.com/lightninglabs/protobuf-hex-display/proto"
)

var (
	jsonMarshaler = &jsonpb.Marshaler{
		EmitDefaults: true,
		OrigName:     true,
	}
)

// PrintMsg prints a protobuf message as JSON, suitable for logging (without any
// indentation).
func PrintMsg(message proto.Message) string {
	jsonStr, err := jsonMarshaler.MarshalToString(message)
	if err != nil {
		return fmt.Sprintf("<unable to decode proto msg: %v>", err)
	}

	return jsonStr
}
