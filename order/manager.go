package order

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/tor"
)

const (
	// defaultLndTimeout is the default number of seconds we are willing to
	// wait for our lnd node to respond.
	defaultLndTimeout = time.Second * 30

	// MinimumOrderDurationBlocks is the minimum for a bid's MinDuration or
	// an ask's MaxDuration.
	MinimumOrderDurationBlocks = 144
)

var (
	// ErrVersionMismatch is the error that is returned if we don't
	// implement the same batch verification version as the server.
	ErrVersionMismatch = fmt.Errorf("version %d mismatches server version",
		CurrentBatchVersion)

	// ErrInvalidBatchHeightHint is an error returned by a trader upon
	// verifying a batch when its proposed height hint is outside of the
	// trader's acceptable range.
	ErrInvalidBatchHeightHint = errors.New("proposed batch height hint is " +
		"outside of acceptable range")
)

// ManagerConfig contains all of the required dependencies for the Manager to
// carry out its duties.
type ManagerConfig struct {
	// Store is responsible for storing and retrieving order information.
	Store Store

	AcctStore account.Store

	// Lightning is used to access the main RPC to get information about the
	// lnd node that poold is connected to.
	Lightning lndclient.LightningClient

	// Wallet is responsible for deriving new keys we need to sign orders.
	Wallet lndclient.WalletKitClient

	// Signer is used to sign orders before submitting them to the server.
	Signer lndclient.SignerClient
}

// Manager is responsible for the management of orders.
type Manager struct {
	// NOTE: This must be used atomically.
	hasPendingBatch uint32
	isStarted       uint32

	started sync.Once
	stopped sync.Once

	cfg ManagerConfig

	wg   sync.WaitGroup
	quit chan struct{}

	ourNodeInfo *lndclient.Info

	batchVerifier BatchVerifier
	batchSigner   BatchSigner
	batchStorer   BatchStorer
	pendingBatch  *Batch
}

// NewManager instantiates a new Manager backed by the given config.
func NewManager(cfg *ManagerConfig) *Manager {
	return &Manager{
		cfg:  *cfg,
		quit: make(chan struct{}),
	}
}

// Start starts all concurrent tasks the manager is responsible for.
func (m *Manager) Start() error {
	if atomic.LoadUint32(&m.isStarted) == 1 {
		return fmt.Errorf("manager can only be started once")
	}

	var err error
	m.started.Do(func() {
		// We'll need our node's identity public key for a bunch of
		// different validations so we might as well cache it on
		// startup as it cannot change.
		ctxt, cancel := context.WithTimeout(
			context.Background(), defaultLndTimeout,
		)
		defer cancel()
		m.ourNodeInfo, err = m.cfg.Lightning.GetInfo(ctxt)
		if err != nil {
			return
		}
		m.batchVerifier = &batchVerifier{
			orderStore:    m.cfg.Store,
			getAccount:    m.cfg.AcctStore.Account,
			wallet:        m.cfg.Wallet,
			ourNodePubkey: m.ourNodeInfo.IdentityPubkey,
		}
		m.batchSigner = &batchSigner{
			getAccount: m.cfg.AcctStore.Account,
			signer:     m.cfg.Signer,
		}
		m.batchStorer = &batchStorer{
			orderStore: m.cfg.Store,
			getAccount: m.cfg.AcctStore.Account,
		}

		atomic.StoreUint32(&m.isStarted, 1)
	})
	return err
}

// Stop stops all concurrent tasks the manager is responsible for.
func (m *Manager) Stop() {
	m.stopped.Do(func() {
		close(m.quit)
		m.wg.Wait()
	})
}

// PrepareOrder validates an order, signs it and then stores it locally.
func (m *Manager) PrepareOrder(ctx context.Context, order Order,
	acct *account.Account,
	terms *terms.AuctioneerTerms) (*ServerOrderParams, error) {

	// Verify incoming request for formal validity.
	err := m.validateOrder(order, acct, terms)
	if err != nil {
		return nil, err
	}

	params := &ServerOrderParams{}
	bid, isBid := order.(*Bid)
	isSidecar := isBid && bid.SidecarTicket != nil

	// Using a sidecar ticket with a bid order means we won't be receiving
	// the channel ourselves and are instead leasing a channel for another
	// node. Therefore we need to add the other node's identity and multisig
	// public key in the bid order we send to the auctioneer.
	if isSidecar {
		ticket := bid.SidecarTicket

		// Make sure the sidecar ticket is in the correct state. It
		// needs to have been registered with the recipient's node and
		// that node's information must be present. If everything checks
		// out, we add our signature over it since we are now sure that
		// we have an order nonce set.
		err := m.validateAndSignTicketForOrder(ctx, ticket, bid, acct)
		if err != nil {
			return nil, fmt.Errorf("error validating sidecar "+
				"ticket: %v", err)
		}

		copy(
			params.NodePubkey[:],
			ticket.Recipient.NodePubKey.SerializeCompressed(),
		)
		copy(
			params.MultiSigKey[:],
			ticket.Recipient.MultiSigPubKey.SerializeCompressed(),
		)

		// Most sidecar recipients won't be public nodes and therefore
		// don't have connectable addresses.
		//
		// TODO(guggero): Add addresses to the Receiver part of the
		// ticket too? Probably won't be used often unless set up with
		// tor by default.
		params.Addrs = nil
	} else {
		// Grab the additional information needed from our local node.
		nextMultiSigKey, err := m.cfg.Wallet.DeriveNextKey(
			ctx, int32(keychain.KeyFamilyMultiSig),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to derive multi sig "+
				"key: %v", err)
		}
		order.Details().MultiSigKeyLocator = nextMultiSigKey.KeyLocator
		copy(
			params.MultiSigKey[:],
			nextMultiSigKey.PubKey.SerializeCompressed(),
		)
		info, err := m.cfg.Lightning.GetInfo(ctx)
		if err != nil {
			return nil, fmt.Errorf("unable to get local node "+
				"info: %v", err)
		}
		params.NodePubkey = info.IdentityPubkey
		params.Addrs, err = parseNodeUris(info.Uris)
		if err != nil {
			return nil, fmt.Errorf("unable to parse node uris: %v",
				err)
		}
	}

	// If the order is a ask, then this means they should be an effective
	// routing node, so we require them to have at least a single
	// advertised address.
	if len(params.Addrs) == 0 && order.Type() == TypeAsk {
		return nil, fmt.Errorf("the lnd node must " +
			"be reachable on clearnet to negotiate channel " +
			"ask order")
	}

	// Sign the order digest with the account key.
	digest, err := order.Digest()
	if err != nil {
		return nil, fmt.Errorf("could not digest "+
			"order: %v", err)
	}
	params.RawSig, err = m.cfg.Signer.SignMessage(
		ctx, digest[:], acct.TraderKey.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to sign "+
			"order: %v", err)
	}

	// There shouldn't be anything that can go wrong on our side, so store
	// the pending order in our local database.
	err = m.cfg.Store.SubmitOrder(order)
	if err != nil {
		return nil, fmt.Errorf("unable to store "+
			"order: %v", err)
	}

	return params, nil
}

// validateOrder makes sure an order is formally correct and that the associated
// account contains enough balance to execute the order.
func (m *Manager) validateOrder(order Order, acct *account.Account,
	terms *terms.AuctioneerTerms) error {

	duration := order.Details().LeaseDuration
	_, ok := terms.LeaseDurationBuckets[duration]
	if !ok {
		return fmt.Errorf("invalid lease duration, must be one of %v",
			terms.LeaseDurationBuckets)
	}

	if order.Details().MaxBatchFeeRate < chainfee.FeePerKwFloor {
		return fmt.Errorf("invalid max batch fee rate %v, must be "+
			"greater than %v", order.Details().MaxBatchFeeRate,
			chainfee.FeePerKwFloor)
	}

	// Check all conditions that come with the use of the self chan balance.
	bid, isBid := order.(*Bid)
	if isBid && bid.SelfChanBalance > 0 {
		if err := bid.ValidateSelfChanBalance(); err != nil {
			return err
		}
	}

	// Get all existing orders.
	dbOrders, err := m.cfg.Store.GetOrders()
	if err != nil {
		return err
	}

	// Ensure the total reserved value won't be larger than the account
	// value when adding this order.
	var acctKey [33]byte
	copy(acctKey[:], acct.TraderKey.PubKey.SerializeCompressed())
	feeSchedule := terms.FeeSchedule()
	reserved := order.ReservedValue(feeSchedule)
	for _, o := range dbOrders {
		// Only tally the reserved balance if this order was submitted
		// by this account.
		if o.Details().AcctKey != acctKey {
			continue
		}

		reserved += o.ReservedValue(feeSchedule)
	}

	if acct.Value < reserved {
		return ErrInsufficientBalance
	}

	return nil
}

// OrderMatchValidate verifies an incoming batch is sane before accepting it.
func (m *Manager) OrderMatchValidate(batch *Batch, bestHeight uint32) error {
	// Make sure we have no objection to the current batch. Then store
	// it in case it ends up being the final version.
	err := m.batchVerifier.Verify(batch, bestHeight)
	if err != nil {
		// This error will lead to us sending an OrderMatchReject
		// message and canceling all funding shims we might already have
		// set up.
		return fmt.Errorf("error validating batch: %w", err)
	}

	m.pendingBatch = batch
	atomic.StoreUint32(&m.hasPendingBatch, 1)

	return nil
}

// HasPendingBatch returns whether a pending batch is currently being processed.
func (m *Manager) HasPendingBatch() bool {
	return atomic.LoadUint32(&m.hasPendingBatch) == 1
}

// PendingBatch returns the current pending batch being validated.
func (m *Manager) PendingBatch() *Batch {
	return m.pendingBatch
}

// BatchSign returns the witness stack of all account inputs in a batch that
// belong to the trader. Before sending off the signature to the auctioneer,
// we'll also persist the batch to disk as pending to ensure we can recover
// after a crash.
func (m *Manager) BatchSign() (BatchSignature, error) {
	sig, err := m.batchSigner.Sign(m.pendingBatch)
	if err != nil {
		return nil, err
	}

	err = m.batchStorer.StorePendingBatch(m.pendingBatch)
	if err != nil {
		return nil, fmt.Errorf("unable to store batch: %v", err)
	}

	return sig, nil
}

// BatchFinalize marks a batch as complete upon receiving the finalize message
// from the auctioneer.
func (m *Manager) BatchFinalize(batchID BatchID) error {
	// Only accept the last batch we verified to make sure we didn't miss
	// a message somewhere in the process.
	if batchID != m.pendingBatch.ID {
		return fmt.Errorf("unexpected batch ID %x, doesn't match last "+
			"validated batch %x", batchID, m.pendingBatch.ID)
	}

	// Create a diff and then persist that. Finally signal that we are ready
	// for the next batch by removing the current pending batch.
	if err := m.batchStorer.MarkBatchComplete(); err != nil {
		return fmt.Errorf("unable to mark batch as complete: %v", err)
	}

	m.pendingBatch = nil
	atomic.StoreUint32(&m.hasPendingBatch, 0)

	return nil
}

// OurNodePubkey returns our lnd node's public identity key or an error if the
// manager wasn't fully started yet.
func (m *Manager) OurNodePubkey() ([33]byte, error) {
	if atomic.LoadUint32(&m.isStarted) != 1 {
		return [33]byte{}, fmt.Errorf("manager not started yet")
	}

	return m.ourNodeInfo.IdentityPubkey, nil
}

// validateAndSignTicketForOrder makes sure that the given sidecar ticket is in
// the correct state for being used in a bid order and has all the necessary
// information set. We also check that our node initially offered to lease this
// channel by checking the embedded signature. If everything checks out, we add
// our signature over the order part to the ticket.
func (m *Manager) validateAndSignTicketForOrder(ctx context.Context,
	t *sidecar.Ticket, bid *Bid, acct *account.Account) error {

	if t.State != sidecar.StateRegistered {
		return fmt.Errorf("invalid sidecar ticket state: %d", t.State)
	}

	// In theory a ticket should never be in the "registered" state if the
	// information in the following checks is missing. But we never know...
	r := t.Recipient
	if r == nil || r.NodePubKey == nil || r.MultiSigPubKey == nil {
		return fmt.Errorf("invalid sidecar ticket, missing recipient " +
			"information")
	}

	// Make sure the offer is valid and actually came from us.
	o := t.Offer
	if err := sidecar.VerifyOffer(ctx, t, m.cfg.Signer); err != nil {
		return fmt.Errorf("error verifying sidecar offer: %v", err)
	}

	if !acct.TraderKey.PubKey.IsEqual(o.SignPubKey) {
		return fmt.Errorf("invalid sidecar ticket, not offered by us")
	}

	// The signature is valid! Let's now make sure the offer and the order
	// parameters actually match.
	err := sidecar.CheckOfferParamsForOrder(
		o, bid.Amt, btcutil.Amount(bid.MinUnitsMatch), BaseSupplyUnit,
	)
	if err != nil {
		return err
	}

	// Everything checks out, let's add our signature to the ticket now.
	return sidecar.SignOrder(
		ctx, t, bid.nonce, acct.TraderKey.KeyLocator,
		m.cfg.Signer,
	)
}

// parseOnionAddr parses an onion address specified in host:port format.
func parseOnionAddr(onionAddr string) (net.Addr, error) {
	addrHost, addrPort, err := net.SplitHostPort(onionAddr)
	if err != nil {
		// If the port wasn't specified, then we'll assume the
		// default p2p port.
		addrHost = onionAddr
		addrPort = "9735" // TODO(roasbeef): constant somewhere?
	}

	portNum, err := strconv.Atoi(addrPort)
	if err != nil {
		return nil, err
	}

	return &tor.OnionAddr{
		OnionService: addrHost,
		Port:         portNum,
	}, nil
}

// parseNodeUris parses a list of node URIs in the format <pubkey>@addr:port
// as it's returned in the `lnrpc.GetInfo` request.
func parseNodeUris(uris []string) ([]net.Addr, error) {
	result := make([]net.Addr, 0, len(uris))
	for _, uri := range uris {
		parts := strings.Split(uri, "@")
		if len(parts) != 2 {
			return nil, fmt.Errorf("node URI not in format " +
				"<pubkey>@addr:port")
		}

		var (
			addr net.Addr
			err  error
		)

		// Obtain the host to determine if this is a Tor address.
		host, _, err := net.SplitHostPort(parts[1])
		if err != nil {
			host = parts[1]
		}

		switch {
		// We'll need to parse onion addresses in a different manner as
		// the encoding also differ from v2 to v3 addrs.
		case tor.IsOnionHost(host):
			addr, err = parseOnionAddr(parts[1])
			if err != nil {
				return nil, err
			}

		// Otherwise, we assumes this is a normal TCP/IP address.  We
		// don't care about the pubkey here, only the address part.
		default:
			addr, err = net.ResolveTCPAddr("tcp", parts[1])
			if err != nil {
				return nil, fmt.Errorf("could not parse "+
					"node URI: %v", err)
			}
		}

		result = append(result, addr)

	}
	return result, nil
}
