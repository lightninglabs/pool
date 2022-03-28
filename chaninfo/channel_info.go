package chaninfo

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chanbackup"
)

// ChannelInfo contains the relevant info required for a third-party to detect a
// channel lifetime violation by either party. These are obtained individually
// from the channel backup of each channel participant.
type ChannelInfo struct {
	// Version is the version of the channel backup. This uniquely
	// identifies the type of channel we're working with.
	Version chanbackup.SingleBackupVersion

	// LocalNodeKey is the node's identifying public key.
	LocalNodeKey *btcec.PublicKey

	// RemoteNodeKey is the remote node's identifying public key.
	RemoteNodeKey *btcec.PublicKey

	// LocalPaymentBasePoint is the node's base public key used within the
	// non-delayed pay-to-self output on the commitment transaction.
	LocalPaymentBasePoint *btcec.PublicKey

	// RemotePaymentBasePoint is the remote node's base public key used
	// within the non-delayed pay-to-self output on the commitment
	// transaction.
	RemotePaymentBasePoint *btcec.PublicKey
}

// Matches determines whether a pair of ChannelInfo correspond to each
// other. This serves as a helper function to ensure channel participants
// provide the auctioneer with the correct expected keys.
func (a *ChannelInfo) Match(b *ChannelInfo) error {
	if a.Version != b.Version {
		return fmt.Errorf("version mismatch: %v vs %v", a.Version,
			b.Version)
	}
	if !a.LocalNodeKey.IsEqual(b.RemoteNodeKey) {
		return fmt.Errorf("node key mismatch %x vs %x",
			a.LocalNodeKey.SerializeCompressed(),
			b.RemoteNodeKey.SerializeCompressed())
	}
	if !a.RemoteNodeKey.IsEqual(b.LocalNodeKey) {
		return fmt.Errorf("node key mismatch %x vs %x",
			a.RemoteNodeKey.SerializeCompressed(),
			b.LocalNodeKey.SerializeCompressed())
	}
	if !a.LocalPaymentBasePoint.IsEqual(b.RemotePaymentBasePoint) {
		return fmt.Errorf("payment base point mismatch %x vs %x",
			a.LocalPaymentBasePoint.SerializeCompressed(),
			b.RemotePaymentBasePoint.SerializeCompressed())
	}
	if !a.RemotePaymentBasePoint.IsEqual(b.LocalPaymentBasePoint) {
		return fmt.Errorf("payment base point mismatch %x vs %x",
			a.RemotePaymentBasePoint.SerializeCompressed(),
			b.LocalPaymentBasePoint.SerializeCompressed())
	}

	return nil
}

// GatherChannelInfo retrieves the relevant channel info from a channel's
// participant point of view required for the auctioneer to enforce channel
// lifetime violations.
func GatherChannelInfo(ctx context.Context, client lndclient.LightningClient,
	wallet lndclient.WalletKitClient,
	channelPoint wire.OutPoint) (*ChannelInfo, error) {

	// First, obtain the node's identifying public key.
	nodeInfo, err := client.GetInfo(ctx)
	if err != nil {
		return nil, err
	}
	localNodeKey, err := btcec.ParsePubKey(nodeInfo.IdentityPubkey[:])
	if err != nil {
		return nil, err
	}

	// We'll obtain the remainder of the information from the node's channel
	// backup.
	channelBackup, err := fetchChannelBackup(
		ctx, client, wallet, channelPoint,
	)
	if err != nil {
		return nil, err
	}

	localPaymentBasePoint, err := wallet.DeriveKey(
		ctx, &channelBackup.LocalChanCfg.PaymentBasePoint.KeyLocator,
	)
	if err != nil {
		return nil, err
	}
	remotePaymentBasePoint := channelBackup.RemoteChanCfg.PaymentBasePoint

	return &ChannelInfo{
		Version:                channelBackup.Version,
		LocalNodeKey:           localNodeKey,
		RemoteNodeKey:          channelBackup.RemoteNodePub,
		LocalPaymentBasePoint:  localPaymentBasePoint.PubKey,
		RemotePaymentBasePoint: remotePaymentBasePoint.PubKey,
	}, nil
}

// fetchChannelBackup retrieves a node's encrypted channel backup for the given
// channel and decrypts it.
func fetchChannelBackup(ctx context.Context, client lndclient.LightningClient,
	wallet lndclient.WalletKitClient,
	channelPoint wire.OutPoint) (*chanbackup.Single, error) {

	scbEncryptionKey, err := wallet.DeriveKey(ctx, &scbEncryptionKeyLocator)
	if err != nil {
		return nil, err
	}

	rawChannelBackup, err := client.ChannelBackup(ctx, channelPoint)
	if err != nil {
		return nil, err
	}

	var channelBackup chanbackup.Single
	err = channelBackup.UnpackFromReader(
		bytes.NewReader(rawChannelBackup), &scbKeyRing{
			encryptionKey: *scbEncryptionKey,
		},
	)
	if err != nil {
		return nil, err
	}

	if !isSupportedBackupVersion(&channelBackup) {
		return nil, fmt.Errorf("unsupported channel backup version %v",
			channelBackup.Version)
	}

	return &channelBackup, nil
}

// isSupportedBackupVersion determines whether the version of the channel backup
// is supported.
func isSupportedBackupVersion(backup *chanbackup.Single) bool {
	switch backup.Version {
	case chanbackup.TweaklessCommitVersion,
		chanbackup.AnchorsCommitVersion,
		chanbackup.AnchorsZeroFeeHtlcTxCommitVersion,
		chanbackup.ScriptEnforcedLeaseVersion:

		return true
	default:
		return false
	}
}
