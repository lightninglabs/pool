package taro

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightninglabs/taro/tarorpc"
	wrpc "github.com/lightninglabs/taro/tarorpc/assetwalletrpc"
	"github.com/lightninglabs/taro/tarorpc/mintrpc"
	"google.golang.org/grpc"
)

var (
	ErrNotInitialized = errors.New(
		"taro daemon client connection not initialized",
	)

	DefaultTimeout = 10 * time.Second
)

type Client interface {
	IsConnected() bool
	PrepareExternalAnchor(*account.Account,
		*btcec.PublicKey) (*txscript.TapLeaf, *mintrpc.MintingBatch,
		error)
	ExternalAnchor(*account.Account, *btcec.PublicKey, *psbt.Packet,
		int32) error
	PrepareReAnchor(
		account *account.Account) (*txscript.TapLeaf, error)
}

type GrpcClient struct {
	timeout    time.Duration
	clientConn *grpc.ClientConn
}

func NewGrpcClient(clientConn *grpc.ClientConn) *GrpcClient {
	return &GrpcClient{
		timeout:    DefaultTimeout,
		clientConn: clientConn,
	}
}

func (c *GrpcClient) IsConnected() bool {
	return c.clientConn != nil
}

func (c *GrpcClient) PrepareExternalAnchor(account *account.Account,
	batchKey *btcec.PublicKey) (*txscript.TapLeaf, *mintrpc.MintingBatch,
	error) {

	if c.clientConn == nil {
		return nil, nil, ErrNotInitialized
	}

	ctxt, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	mintClient := mintrpc.NewMintClient(c.clientConn)
	if err := checkBatchExists(ctxt, mintClient, batchKey); err != nil {
		return nil, nil, err
	}

	prepareResp, err := mintClient.PrepareExternalAnchor(
		ctxt, &mintrpc.PrepareExternalAnchorRequest{
			BatchKey: batchKey.SerializeCompressed(),
			GenesisOutpoint: &tarorpc.OutPoint{
				Hash:  account.OutPoint.Hash[:],
				Index: account.OutPoint.Index,
			},
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("error preparing external anchor: "+
			"%w", err)
	}

	return parseTapLeaf(prepareResp.TaroTapLeaf), prepareResp.Batch, nil
}

func (c *GrpcClient) ExternalAnchor(account *account.Account,
	batchKey *btcec.PublicKey, fundedPkt *psbt.Packet,
	changeOutputIndex int32) error {

	if c.clientConn == nil {
		return ErrNotInitialized
	}

	ctxt, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	mintClient := mintrpc.NewMintClient(c.clientConn)
	if err := checkBatchExists(ctxt, mintClient, batchKey); err != nil {
		return err
	}

	var buf bytes.Buffer
	if err := fundedPkt.Serialize(&buf); err != nil {
		return fmt.Errorf("error serializing psbt: %w", err)
	}

	internalKey, expiryLeaf, err := poolscript.TaprootKey(
		account.Version.ScriptVersion(), account.Expiry,
		account.TraderKey.PubKey, account.AuctioneerKey,
		account.BatchKey, account.Secret, account.TaroLeaf,
	)
	if err != nil {
		return fmt.Errorf("error deriving account internal key: %w",
			err)
	}

	// Find out which was the actual account output and decorate it with the
	// internal key.
	acctOutputIndex := account.OutPoint.Index
	rawInternalKey := internalKey.PreTweakedKey.SerializeCompressed()
	fundedPkt.Outputs[acctOutputIndex].TaprootInternalKey = rawInternalKey

	// As long as there are only two leaves, the order is not important as
	// they are going to be sorted lexicographically by their hash anyway.
	leaves := []*tarorpc.TapLeaf{{
		Version: uint32(account.TaroLeaf.LeafVersion),
		Script:  account.TaroLeaf.Script,
	}, {
		Version: uint32(expiryLeaf.LeafVersion),
		Script:  expiryLeaf.Script,
	}}

	_, err = mintClient.ExternalAnchor(ctxt, &mintrpc.ExternalAnchorRequest{
		BatchKey:          batchKey.SerializeCompressed(),
		FundedPacket:      buf.Bytes(),
		ChangeOutputIndex: changeOutputIndex,
		AnchorOutputIndex: acctOutputIndex,
		AnchorInternalKey: rawInternalKey,
		AnchorTaprootTree: leaves,
	})
	return err
}

func (c *GrpcClient) PrepareReAnchor(
	account *account.Account) (*txscript.TapLeaf, error) {

	if c.clientConn == nil {
		return nil, ErrNotInitialized
	}

	ctxt, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	walletClient := wrpc.NewAssetWalletClient(c.clientConn)
	prepareResp, err := walletClient.PrepareReAnchor(
		ctxt, &wrpc.PrepareReAnchorRequest{
			Outpoint: &tarorpc.OutPoint{
				Hash:  account.OutPoint.Hash[:],
				Index: account.OutPoint.Index,
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error preparing re-anchor: %w", err)
	}

	return parseTapLeaf(prepareResp.TaroTapLeaf), nil
}

func checkBatchExists(ctx context.Context, mintClient mintrpc.MintClient,
	batchKey *btcec.PublicKey) error {

	batches, err := mintClient.ListBatches(
		ctx, &mintrpc.ListBatchRequest{},
	)
	if err != nil {
		return fmt.Errorf("error looking up taro batches: %w", err)
	}

	var (
		found         = false
		batchKeyBytes = batchKey.SerializeCompressed()
	)
	for idx := range batches.Batches {
		batch := batches.Batches[idx]
		if bytes.Equal(batch.BatchKey, batchKeyBytes) {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("minting batch with key %x not found in "+
			"list of tarod batches", batchKeyBytes)
	}

	return nil
}

// parseTapLeaf parses a TapLeaf from the RPC representation.
func parseTapLeaf(rpcLeaf *tarorpc.TapLeaf) *txscript.TapLeaf {
	return &txscript.TapLeaf{
		LeafVersion: txscript.TapscriptLeafVersion(rpcLeaf.Version),
		Script:      rpcLeaf.Script,
	}
}
