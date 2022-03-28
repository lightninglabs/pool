package test

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

// CreateKey returns a deterministically generated key pair.
func CreateKey(index int32) (*btcec.PrivateKey, *btcec.PublicKey) {
	// Avoid all zeros, because it results in an invalid key.
	privKey, pubKey := btcec.PrivKeyFromBytes([]byte{
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(index + 1),
	})

	return privKey, pubKey
}

// NewSignatureFromInt creates a new signature from uint32 scalar values.
func NewSignatureFromInt(r, s uint32) *ecdsa.Signature {
	testRScalar := new(btcec.ModNScalar).SetInt(r)
	testSScalar := new(btcec.ModNScalar).SetInt(s)

	return ecdsa.NewSignature(testRScalar, testSScalar)
}
