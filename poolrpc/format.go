package poolrpc

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	// ProtoJSONMarshalOpts is a struct that holds the default marshal
	// options for marshaling protobuf messages into JSON in a
	// human-readable way. This should only be used in the CLI and in
	// integration tests.
	ProtoJSONMarshalOpts = &protojson.MarshalOptions{
		EmitUnpopulated: true,
		UseProtoNames:   true,
		Indent:          "    ",
		UseHexForBytes:  true,
	}
)

// PrintMsg prints a protobuf message as JSON, suitable for logging (without any
// indentation).
func PrintMsg(message proto.Message) string {
	jsonBytes, err := ProtoJSONMarshalOpts.Marshal(message)
	if err != nil {
		return fmt.Sprintf("<unable to decode proto msg: %v>", err)
	}

	return string(jsonBytes)
}
