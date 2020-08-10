package test

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	testStartingHeight = int32(600)
	testNodePubkey     = "03f5374b16f0b1f1b49101de1b9d89e0b460bc57ce9c2f9" +
		"132b73dfc76d3704daa"
	testSignature    = []byte{55, 66, 77, 88, 99}
	testSignatureMsg = "test"
)

// SignOutputRawRequest contains input data for a tx signing request.
type SignOutputRawRequest struct {
	Tx              *wire.MsgTx
	SignDescriptors []*lndclient.SignDescriptor
}

func NewMockSigner() *MockSigner {
	return &MockSigner{
		SignOutputRawChannel: make(chan SignOutputRawRequest),
		Height:               testStartingHeight,
		NodePubkey:           testNodePubkey,
		Signature:            testSignature,
		SignatureMsg:         testSignatureMsg,
	}
}

type MockSigner struct {
	SignOutputRawChannel chan SignOutputRawRequest

	Height       int32
	NodePubkey   string
	Signature    []byte
	SignatureMsg string
}

var _ lndclient.SignerClient = (*MockSigner)(nil)

func (s *MockSigner) SignOutputRaw(_ context.Context, tx *wire.MsgTx,
	signDescriptors []*lndclient.SignDescriptor) ([][]byte, error) {

	s.SignOutputRawChannel <- SignOutputRawRequest{
		Tx:              tx,
		SignDescriptors: signDescriptors,
	}

	rawSigs := [][]byte{{1, 2, 3}}

	return rawSigs, nil
}

func (s *MockSigner) ComputeInputScript(context.Context, *wire.MsgTx,
	[]*lndclient.SignDescriptor) ([]*input.Script, error) {

	return nil, fmt.Errorf("unimplemented")
}

func (s *MockSigner) SignMessage(context.Context, []byte,
	keychain.KeyLocator) ([]byte, error) {

	return s.Signature, nil
}

func (s *MockSigner) VerifyMessage(_ context.Context, msg, sig []byte,
	_ [33]byte) (bool, error) {

	// Make the mock somewhat functional by asserting that the message and
	// signature is what we expect from the mock parameters.
	mockAssertion := bytes.Equal(msg, []byte(s.SignatureMsg)) &&
		bytes.Equal(sig, s.Signature)

	return mockAssertion, nil
}

func (s *MockSigner) DeriveSharedKey(context.Context, *btcec.PublicKey,
	*keychain.KeyLocator) ([32]byte, error) {

	return [32]byte{4, 5, 6}, nil
}
