package chaninfo

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec"
)

// TestMatchChannelInfo ensures that we properly match a ChannelInfo pair.
func TestMatchChannelInfo(t *testing.T) {
	t.Parallel()

	// Construct two distinct instances of each key to use for the two
	// ChannelInfo instances.
	localNodeKeyBytes, _ := hex.DecodeString(
		"02187d1a0e30f4e5016fc1137363ee9e7ed5dde1e6c50f367422336df7a108b716",
	)
	localNodeKey1, _ := btcec.ParsePubKey(localNodeKeyBytes, btcec.S256())
	localNodeKey2, _ := btcec.ParsePubKey(localNodeKeyBytes, btcec.S256())

	remoteNodeKeyBytes, _ := hex.DecodeString(
		"02929da6f6fa95c0a4a7dcc95dfa4aebaeda654819324f8b62f0c4ef34812d0c7a",
	)
	remoteNodeKey1, _ := btcec.ParsePubKey(remoteNodeKeyBytes, btcec.S256())
	remoteNodeKey2, _ := btcec.ParsePubKey(remoteNodeKeyBytes, btcec.S256())

	localPaymentBasePointBytes, _ := hex.DecodeString(
		"0228efbf608dbbeb90684fa3cf22c69dfaf491edddd6d87a051f8d2ef1ae28b95c",
	)
	localPaymentBasePoint1, _ := btcec.ParsePubKey(
		localPaymentBasePointBytes, btcec.S256(),
	)
	localPaymentBasePoint2, _ := btcec.ParsePubKey(
		localPaymentBasePointBytes, btcec.S256(),
	)

	remotePaymentBasePointBytes, _ := hex.DecodeString(
		"02ea91d9ffa39b90da87aa9aad5d7f5b2085bffb4f1b9be74d0bfedea2aefa0729",
	)
	remotePaymentBasePoint1, _ := btcec.ParsePubKey(
		remotePaymentBasePointBytes, btcec.S256(),
	)
	remotePaymentBasePoint2, _ := btcec.ParsePubKey(
		remotePaymentBasePointBytes, btcec.S256(),
	)

	a := &ChannelInfo{
		Version:                1,
		LocalNodeKey:           localNodeKey1,
		RemoteNodeKey:          remoteNodeKey1,
		LocalPaymentBasePoint:  localPaymentBasePoint1,
		RemotePaymentBasePoint: remotePaymentBasePoint1,
	}
	b := &ChannelInfo{
		Version:                1,
		LocalNodeKey:           remoteNodeKey2,
		RemoteNodeKey:          localNodeKey2,
		LocalPaymentBasePoint:  remotePaymentBasePoint2,
		RemotePaymentBasePoint: localPaymentBasePoint2,
	}
	if err := a.Match(b); err != nil {
		t.Fatalf("unexpected mismatch: %v", err)
	}

	// Modify the version of one, we should expect to see a failure.
	a.Version = 2
	if err := a.Match(b); err == nil {
		t.Fatal("expected mismatch")
	}
}
