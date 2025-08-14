package order

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

var nodeIDSerializationTestCases = []struct {
	name                  string
	nodeIDs               func() [][33]byte
	invalidSerializedData func() [][]byte
	expectedErr           string
}{{
	name: "empty slice",
	nodeIDs: func() [][33]byte {
		return [][33]byte{}
	},
}, {
	name: "single node id",
	nodeIDs: func() [][33]byte {
		return [][33]byte{
			nodePubkey,
		}
	},
}, {
	name: "multiple node ids",
	nodeIDs: func() [][33]byte {
		nodeID, _ := hex.DecodeString("036b51e0cc2d9e5988ee4967e0ba67" +
			"ef3727bb633fea21a0af58e0c9395446ba09")
		var nodePubKey2 [33]byte
		copy(nodePubKey2[:], nodeID)

		return [][33]byte{
			nodePubkey,
			nodePubKey2,
		}
	},
}, {
	name: "invalid length",
	invalidSerializedData: func() [][]byte {
		return [][]byte{
			{1, 2},
		}
	},
	expectedErr: "invalid node_id length",
}, {
	name: "invalid pub key",
	invalidSerializedData: func() [][]byte {
		return MarshalNodeIDSlice([][33]byte{
			{1, 2},
		})
	},
	expectedErr: "invalid node_id:",
}}

// TestNodeIDSliceSerialization tests that we can properly serialize and
// deserialize a slice of node ids.
func TestNodeIDSliceSerialization(t *testing.T) {
	for _, tc := range nodeIDSerializationTestCases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			switch {
			// Marshal and Unmarshal valid node ids.
			case tc.nodeIDs != nil:
				nodeIDs := tc.nodeIDs()
				marshaled := MarshalNodeIDSlice(nodeIDs)
				require.Equal(t, len(nodeIDs), len(marshaled))

				unmarshaled, err := UnmarshalNodeIDSlice(
					marshaled,
				)

				require.NoError(t, err)
				require.Equal(t, tc.nodeIDs(), unmarshaled)

			// Unmarshal invalid marshaled node ids.
			case tc.invalidSerializedData != nil:
				marshaled := tc.invalidSerializedData()

				_, err := UnmarshalNodeIDSlice(marshaled)
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.expectedErr)

			default:
				require.Fail(t, "invalid test case")
			}
		})
	}
}
