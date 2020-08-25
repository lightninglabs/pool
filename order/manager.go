package order

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightninglabs/llm/account"
	"github.com/lightninglabs/llm/terms"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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
		CurrentVersion)
)

// ManagerConfig contains all of the required dependencies for the Manager to
// carry out its duties.
type ManagerConfig struct {
	// Store is responsible for storing and retrieving order information.
	Store Store

	AcctStore account.Store

	// Lightning is used to access the main RPC to get information about the
	// lnd node that llmd is connected to.
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

	started sync.Once
	stopped sync.Once

	cfg ManagerConfig

	wg   sync.WaitGroup
	quit chan struct{}

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
	var err error
	m.started.Do(func() {
		// We'll need our node's identity public key for a bunch of
		// different validations so we might as well cache it on
		// startup as it cannot change.
		var info *lndclient.Info
		ctxt, cancel := context.WithTimeout(
			context.Background(), defaultLndTimeout,
		)
		defer cancel()
		info, err = m.cfg.Lightning.GetInfo(ctxt)
		if err != nil {
			return
		}
		m.batchVerifier = &batchVerifier{
			orderStore:    m.cfg.Store,
			getAccount:    m.cfg.AcctStore.Account,
			wallet:        m.cfg.Wallet,
			ourNodePubkey: info.IdentityPubkey,
		}
		m.batchSigner = &batchSigner{
			getAccount: m.cfg.AcctStore.Account,
			signer:     m.cfg.Signer,
		}
		m.batchStorer = &batchStorer{
			orderStore: m.cfg.Store,
			getAccount: m.cfg.AcctStore.Account,
		}
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

	// Grab the additional information needed from our local node.
	nextMultiSigKey, err := m.cfg.Wallet.DeriveNextKey(
		ctx, int32(keychain.KeyFamilyMultiSig),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive "+
			"multi sig key: %v", err)
	}
	order.Details().MultiSigKeyLocator = nextMultiSigKey.KeyLocator
	var multiSigKey [33]byte
	copy(multiSigKey[:], nextMultiSigKey.PubKey.SerializeCompressed())
	info, err := m.cfg.Lightning.GetInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to get local "+
			"node info: %v", err)
	}
	nodeAddrs, err := parseNodeUris(info.Uris)
	if err != nil {
		return nil, fmt.Errorf("unable to parse "+
			"node uris: %v", err)
	}

	// If the order is a ask, then this means they should be an effective
	// routing node, so we require them to have at least a single
	// advertised address.
	if len(nodeAddrs) == 0 && order.Type() == TypeAsk {
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
	rawSig, err := m.cfg.Signer.SignMessage(
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

	return &ServerOrderParams{
		NodePubkey:  info.IdentityPubkey,
		Addrs:       nodeAddrs,
		RawSig:      rawSig,
		MultiSigKey: multiSigKey,
	}, nil
}

// validateOrder makes sure an order is formally correct and that the associated
// account contains enough balance to execute the order.
func (m *Manager) validateOrder(order Order, acct *account.Account,
	terms *terms.AuctioneerTerms) error {

	// First parse order type specific fields.
	switch o := order.(type) {
	case *Ask:
		if o.MaxDuration < MinimumOrderDurationBlocks {
			return fmt.Errorf("invalid max duration, must be "+
				"at least %d", MinimumOrderDurationBlocks)
		}
		if o.MaxDuration > terms.MaxOrderDuration {
			return fmt.Errorf("invalid max duration, must be "+
				"smaller than or equal to %d",
				terms.MaxOrderDuration)
		}
		if o.MaxDuration%MinimumOrderDurationBlocks != 0 {
			return fmt.Errorf("invalid max duration, must be "+
				"multiple of %d", MinimumOrderDurationBlocks)
		}

	case *Bid:
		if o.MinDuration < MinimumOrderDurationBlocks {
			return fmt.Errorf("invalid min duration, must be "+
				"at least %d", MinimumOrderDurationBlocks)
		}
		if o.MinDuration > terms.MaxOrderDuration {
			return fmt.Errorf("invalid min duration, must be "+
				"smaller than or equal to %d",
				terms.MaxOrderDuration)
		}
		if o.MinDuration%MinimumOrderDurationBlocks != 0 {
			return fmt.Errorf("invalid min duration, must be "+
				"multiple of %d", MinimumOrderDurationBlocks)
		}

	default:
		return fmt.Errorf("invalid order type: %v", o)
	}

	if order.Details().MaxBatchFeeRate < chainfee.FeePerKwFloor {
		return fmt.Errorf("invalid max batch fee rate %v, must be "+
			"greater than %v", order.Details().MaxBatchFeeRate,
			chainfee.FeePerKwFloor)
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
		// Only tally the reserved balance if this order waas submited
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
func (m *Manager) OrderMatchValidate(batch *Batch) error {
	// Make sure we have no objection to the current batch. Then store
	// it in case it ends up being the final version.
	err := m.batchVerifier.Verify(batch)
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
func (m *Manager) BatchSign(bestHeight uint32) (BatchSignature, error) {
	sig, err := m.batchSigner.Sign(m.pendingBatch)
	if err != nil {
		return nil, err
	}

	err = m.batchStorer.StorePendingBatch(m.pendingBatch, bestHeight)
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

// parseNodeUris parses a list of node URIs in the format <pubkey>@addr:port
// as it's returned in the `lnrpc.GetInfo` request.
// TODO(guggero): What is needed to support tor as well?
func parseNodeUris(uris []string) ([]net.Addr, error) {
	result := make([]net.Addr, 0, len(uris))
	for _, uri := range uris {
		parts := strings.Split(uri, "@")
		if len(parts) != 2 {
			return nil, fmt.Errorf("node URI not in format " +
				"<pubkey>@addr:port")
		}

		// We don't currently support tor addresses.
		if strings.Contains(parts[1], ".onion") {
			continue
		}

		// We don't care about the pubkey here, only the address part.
		addr, err := net.ResolveTCPAddr("tcp", parts[1])
		if err != nil {
			return nil, fmt.Errorf("could not parse node URI: %v",
				err)
		}
		result = append(result, addr)
	}
	return result, nil
}
