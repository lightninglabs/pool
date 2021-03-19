package pool

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightningnetwork/lnd/keychain"
)

// SidecarAcceptor is a type that is exclusively responsible for managing the
// recipient's tasks of executing a sidecar channel. The two tasks are:
// 1. Verify a sidecar ticket and the offer contained within then add the
//    recipient node information to the ticket so it can be returned to the
//    sidecar provider. This is step 2/4 of the entire sidecar execution
//    protocol.
// 2. Interact with the auction server and connect out to an asker's node in the
//    right moment then accept the incoming channel. This is step 4/4 of the
//    entire sidecar execution protocol.
// The code for these two tasks are kept separate from the default funding
// manager to make it easier to extract a standalone sidecar acceptor client
// later on. It also makes it easier to see what code would need to be re-
// implemented in another language to integrate just the acceptor part.
type SidecarAcceptor struct {
	store      sidecar.Store
	signer     lndclient.SignerClient
	wallet     lndclient.WalletKitClient
	nodePubKey *btcec.PublicKey
	acceptor   *ChannelAcceptor

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewSidecarAcceptor creates a new sidecar acceptor.
func NewSidecarAcceptor(store sidecar.Store, signer lndclient.SignerClient,
	wallet lndclient.WalletKitClient, acceptor *ChannelAcceptor,
	nodePubKey *btcec.PublicKey) *SidecarAcceptor {

	return &SidecarAcceptor{
		store:      store,
		signer:     signer,
		wallet:     wallet,
		nodePubKey: nodePubKey,
		acceptor:   acceptor,
		quit:       make(chan struct{}),
	}
}

// Start starts the sidecar acceptor.
func (a *SidecarAcceptor) Start(errChan chan error) error {
	return a.acceptor.Start(errChan)
}

// Stop stops the sidecar acceptor.
func (a *SidecarAcceptor) Stop() {
	a.acceptor.Stop()
	close(a.quit)

	a.wg.Wait()
}

// RegisterSidecar derives a new multisig key for a potential future channel
// bought over a sidecar order and adds that to the offered ticket. If
// successful, the updated ticket is added to the local database.
func (a *SidecarAcceptor) RegisterSidecar(ctx context.Context,
	ticket *sidecar.Ticket) error {

	// The ticket needs to be in the correct state for us to register it.
	if err := sidecar.VerifyOffer(ctx, ticket, a.signer); err != nil {
		return fmt.Errorf("error verifying sidecar offer: %v", err)
	}

	// Do we already have a ticket with that ID?
	_, err := a.store.Sidecar(ticket.ID, ticket.Offer.SignPubKey)
	if err != clientdb.ErrNoSidecar {
		return fmt.Errorf("ticket with ID %x already exists",
			ticket.ID[:])
	}

	// First we'll need a new multisig key for the channel that will be
	// opened through this sidecar order.
	keyDesc, err := a.wallet.DeriveNextKey(
		ctx, int32(keychain.KeyFamilyMultiSig),
	)
	if err != nil {
		return fmt.Errorf("error deriving multisig key: %v", err)
	}

	ticket.State = sidecar.StateRegistered
	ticket.Recipient = &sidecar.Recipient{
		NodePubKey:       a.nodePubKey,
		MultiSigPubKey:   keyDesc.PubKey,
		MultiSigKeyIndex: keyDesc.Index,
	}
	if err := a.store.AddSidecar(ticket); err != nil {
		return fmt.Errorf("error storing sidecar: %v", err)
	}

	return nil
}
