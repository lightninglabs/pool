package poolrpc

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/protobuf/proto"
)

// PrintMsg prints a protobuf message as JSON, suitable for logging (without any
// indentation).
func PrintMsg(message proto.Message) string {
	jsonBytes, err := lnrpc.ProtoJSONMarshalOpts.Marshal(message)
	if err != nil {
		return fmt.Sprintf("<unable to decode proto msg: %v>", err)
	}

	return string(jsonBytes)
}
