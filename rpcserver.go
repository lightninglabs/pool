package pool

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/chaninfo"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/event"
	"github.com/lightninglabs/pool/funding"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/tor"
)

const (
	// getInfoTimeout is the maximum time we allow for the initial getInfo
	// call to the connected lnd node.
	getInfoTimeout = 5 * time.Second
)

// rpcServer implements the gRPC server on the client side and answers RPC calls
// from an end user client program like the command line interface.
type rpcServer struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	// bestHeight is the best known height of the main chain. This MUST be
	// used atomically.
	bestHeight uint32

	server         *Server
	lndServices    *lndclient.LndServices
	lndClient      lnrpc.LightningClient
	auctioneer     *auctioneer.Client
	accountManager *account.Manager
	orderManager   *order.Manager

	quit                           chan struct{}
	wg                             sync.WaitGroup
	blockNtfnCancel                func()
	pendingOpenChannelStreamCancel func()
	recoveryMutex                  sync.Mutex
	recoveryPending                bool

	// wumboSupported is true if the backing lnd node supports wumbo
	// channels.
	wumboSupported bool
}

// accountStore is a clientdb.DB wrapper to implement the account.Store
// interface.
type accountStore struct {
	*clientdb.DB
}

var _ account.Store = (*accountStore)(nil)

func (s *accountStore) PendingBatch() error {
	_, err := s.DB.PendingBatchSnapshot()
	return err
}

// newRPCServer creates a new client-side RPC server that uses the given
// connection to the trader's lnd node and the auction server. A client side
// database is created in `serverDir` if it does not yet exist.
func newRPCServer(server *Server) *rpcServer {
	accountStore := &accountStore{server.db}
	lndServices := &server.lndServices.LndServices
	return &rpcServer{
		server:      server,
		lndServices: lndServices,
		lndClient:   server.lndClient,
		auctioneer:  server.AuctioneerClient,
		accountManager: account.NewManager(&account.ManagerConfig{
			Store:          accountStore,
			Auctioneer:     server.AuctioneerClient,
			Wallet:         lndServices.WalletKit,
			Signer:         lndServices.Signer,
			ChainNotifier:  lndServices.ChainNotifier,
			TxSource:       lndServices.Client,
			TxFeeEstimator: lndServices.Client,
			TxLabelPrefix:  server.cfg.TxLabelPrefix,
		}),
		orderManager: order.NewManager(&order.ManagerConfig{
			Store:     server.db,
			AcctStore: accountStore,
			Lightning: lndServices.Client,
			Wallet:    lndServices.WalletKit,
			Signer:    lndServices.Signer,
		}),
		quit: make(chan struct{}),
	}
}

// Start starts the rpcServer, making it ready to accept incoming requests.
func (s *rpcServer) Start() error {
	if !atomic.CompareAndSwapUint32(&s.started, 0, 1) {
		return nil
	}

	rpcLog.Infof("Starting trader server")

	ctx := context.Background()

	lndCtx, lndCancel := context.WithTimeout(ctx, getInfoTimeout)
	defer lndCancel()
	info, err := s.lndClient.GetInfo(lndCtx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return fmt.Errorf("error in GetInfo: %v", err)
	}

	_, s.wumboSupported = info.Features[uint32(lnwire.WumboChannelsRequired)]
	if !s.wumboSupported {
		_, s.wumboSupported = info.Features[uint32(lnwire.WumboChannelsOptional)]
	}

	rpcLog.Infof("Connected to lnd node %v with pubkey %v", info.Alias,
		info.IdentityPubkey)

	var blockCtx context.Context
	blockCtx, s.blockNtfnCancel = context.WithCancel(ctx)
	chainNotifier := s.lndServices.ChainNotifier
	blockChan, blockErrChan, err := chainNotifier.RegisterBlockEpochNtfn(
		blockCtx,
	)
	if err != nil {
		return err
	}

	var height int32
	select {
	case height = <-blockChan:
	case err := <-blockErrChan:
		return fmt.Errorf("unable to receive first block "+
			"notification: %v", err)
	case <-ctx.Done():
		return nil
	}

	s.updateHeight(height)

	// Subscribe to pending open channel notifications. This will be useful
	// when we're creating channels with a matched order as part of a batch.
	streamCtx, streamCancel := context.WithCancel(ctx)
	s.pendingOpenChannelStreamCancel = streamCancel
	subStream, err := s.lndClient.SubscribeChannelEvents(
		streamCtx, &lnrpc.ChannelEventSubscription{},
	)
	if err != nil {
		return err
	}

	// Start the auctioneer client first to establish a connection.
	if err := s.auctioneer.Start(); err != nil {
		return fmt.Errorf("unable to start auctioneer client: %v", err)
	}

	// Start managers.
	if err := s.accountManager.Start(); err != nil {
		return fmt.Errorf("unable to start account manager: %v", err)
	}
	if err := s.orderManager.Start(); err != nil {
		return fmt.Errorf("unable to start order manager: %v", err)
	}

	s.wg.Add(2)
	go s.serverHandler(blockChan, blockErrChan)
	go s.consumePendingOpenChannels(subStream)

	rpcLog.Infof("Trader server is now active")

	return nil
}

// consumePendingOpenChannels consumes pending open channel events from the
// stream and notifies them if the trader currently has an ongoing batch.
func (s *rpcServer) consumePendingOpenChannels(
	subStream lnrpc.Lightning_SubscribeChannelEventsClient) {

	defer s.wg.Done()

	for {
		select {
		case <-s.quit:
			return
		default:
		}

		msg, err := subStream.Recv()
		if err != nil {
			select {
			case <-s.quit:
				return
			default:
			}

			rpcLog.Errorf("Unable to read channel event: %v", err)
			continue
		}

		// Skip any events other than the pending open channel one.
		channel, ok := msg.Channel.(*lnrpc.ChannelEventUpdate_PendingOpenChannel)
		if !ok {
			continue
		}

		// If we don't have a pending batch, then there's no need to
		// notify any pending open channels..
		if !s.orderManager.HasPendingBatch() {
			continue
		}

		select {
		case s.server.fundingManager.PendingOpenChannels <- channel:
		case <-s.quit:
			return
		}
	}
}

// Stop stops the server.
func (s *rpcServer) Stop() error {
	if !atomic.CompareAndSwapUint32(&s.stopped, 0, 1) {
		return nil
	}

	rpcLog.Info("Trader server stopping")
	s.accountManager.Stop()
	s.orderManager.Stop()
	if err := s.auctioneer.Stop(); err != nil {
		rpcLog.Errorf("Error closing server stream: %v")
	}

	close(s.quit)

	// We call this before Wait to ensure the goroutine is stopped by this
	// call.
	s.pendingOpenChannelStreamCancel()

	s.wg.Wait()
	s.blockNtfnCancel()

	rpcLog.Info("Stopped trader server")
	return nil
}

// serverHandler is the main event loop of the server.
func (s *rpcServer) serverHandler(blockChan chan int32, blockErrChan chan error) {
	defer s.wg.Done()

	for {
		select {
		case msg := <-s.auctioneer.FromServerChan:
			// An empty message means the client is shutting down.
			if msg == nil {
				continue
			}

			rpcLog.Debugf("Received message from the server: %v", msg)
			err := s.handleServerMessage(msg)

			// Only shut down if this was a terminal error, and not
			// a batch reject (should rarely happen, but it's
			// possible).
			if err != nil && !errors.Is(err, order.ErrMismatchErr) {
				rpcLog.Errorf("Error handling server message: %v",
					err)
				signal.RequestShutdown()
			}

		case err := <-s.auctioneer.StreamErrChan:
			// If the server is shutting down, then the client has
			// already scheduled a restart. We only need to handle
			// other errors here.
			if err != nil && err != auctioneer.ErrServerShutdown {
				rpcLog.Errorf("Error in server stream: %v", err)
				err := s.auctioneer.HandleServerShutdown(err)
				if err != nil {
					rpcLog.Errorf("Error closing stream: %v",
						err)
				}
			}

		case height := <-blockChan:
			rpcLog.Infof("Received new block notification: height=%v",
				height)
			s.updateHeight(height)

		case err := <-blockErrChan:
			if err != nil {
				rpcLog.Errorf("Unable to receive block "+
					"notification: %v", err)
				signal.RequestShutdown()
			}

		// In case the server is shutting down.
		case <-s.quit:
			return
		}
	}
}

func (s *rpcServer) updateHeight(height int32) {
	// Store height atomically so the incoming request handler can access it
	// without locking.
	atomic.StoreUint32(&s.bestHeight, uint32(height))
}

// handleServerMessage reads a gRPC message received in the stream from the
// auctioneer server and passes it to the correct manager.
func (s *rpcServer) handleServerMessage(rpcMsg *poolrpc.ServerAuctionMessage) error {
	switch msg := rpcMsg.Msg.(type) {
	// A new batch has been assembled with some of our orders.
	case *poolrpc.ServerAuctionMessage_Prepare:
		// Parse and formally validate what we got from the server.
		rpcLog.Tracef("Received prepare msg from server, batch_id=%x: %v",
			msg.Prepare.BatchId, spew.Sdump(msg))
		batch, err := order.ParseRPCBatch(msg.Prepare)
		if err != nil {
			// If we aren't able to parse the batch for some
			// reason, then we'll send a reject message.
			log.Error("unable to parse batch: %v", err)
			return s.sendRejectBatch(batch, err)
		}

		rpcLog.Infof("Received PrepareMsg for batch=%x, num_orders=%v",
			batch.ID[:], len(batch.MatchedOrders))

		// Let's store an event for each order in the batch that we did
		// receive a prepare message.
		if err := s.server.db.StoreBatchEvents(
			batch, order.MatchStatePrepare,
			poolrpc.MatchRejectReason_NONE,
		); err != nil {
			rpcLog.Errorf("Unable to store order events: %v", err)
		}

		// The prepare message can be sent over and over again if the
		// batch needs adjustment. Clear all previous shims and channels
		// that will never complete because the funding TX they refer to
		// will never be published.
		if s.orderManager.HasPendingBatch() {
			pendingBatch := s.orderManager.PendingBatch()
			err = s.server.fundingManager.RemovePendingBatchArtifacts(
				pendingBatch.MatchedOrders, pendingBatch.BatchTX,
			)
			if err != nil {
				// The above method only returns hard errors
				// that justify us rejecting the batch.
				rpcLog.Errorf("Error clearing previous batch "+
					"artifacts: %v", err)
				return s.sendRejectBatch(batch, err)
			}
		}

		// Do an in-depth verification of the batch.
		err = s.orderManager.OrderMatchValidate(batch)
		if err != nil {
			// We can't accept the batch, something went wrong.
			rpcLog.Errorf("Error validating batch: %v", err)
			return s.sendRejectBatch(batch, err)
		}

		// As we need to change our behavior if the node has any Tor
		// addresses, we'll fetch the current state of our advertised
		// addrs now.
		nodeInfo, err := s.lndServices.Client.GetInfo(
			context.Background(),
		)
		if err != nil {
			log.Errorf("error in GetInfo: %v", err)
			return s.sendRejectBatch(
				batch, fmt.Errorf("internal error"),
			)
		}

		// Before we accept the batch, we'll finish preparations on our
		// end which include applying any order match predicates,
		// connecting out to peers, and registering funding shim.
		err = s.server.fundingManager.PrepChannelFunding(
			batch, nodeHasTorAddrs(nodeInfo.Uris), s.quit,
		)
		if err != nil {
			rpcLog.Warnf("Error preparing channel funding: %v",
				err)
			return s.sendRejectBatch(batch, err)
		}

		// Accept the match now.
		err = s.sendAcceptBatch(batch)
		if err != nil {
			rpcLog.Errorf("Error sending accept msg: %v", err)
			return s.sendRejectBatch(batch, err)
		}

	case *poolrpc.ServerAuctionMessage_Sign:
		// We were able to accept the batch. Inform the auctioneer,
		// then start negotiating with the remote peers. We'll sign
		// once all channel partners have responded.
		batch := s.orderManager.PendingBatch()
		channelKeys, err := s.server.fundingManager.BatchChannelSetup(
			batch, s.quit,
		)
		if err != nil {
			rpcLog.Errorf("Error setting up channels: %v", err)
			return s.sendRejectBatch(batch, err)
		}

		rpcLog.Infof("Received OrderMatchSignBegin for batch=%x, "+
			"num_orders=%v", batch.ID[:], len(batch.MatchedOrders))

		// Sign for the accounts in the batch.
		bestHeight := atomic.LoadUint32(&s.bestHeight)
		sigs, err := s.orderManager.BatchSign(bestHeight)
		if err != nil {
			rpcLog.Errorf("Error signing batch: %v", err)
			return s.sendRejectBatch(batch, err)
		}
		err = s.sendSignBatch(batch, sigs, channelKeys)
		if err != nil {
			rpcLog.Errorf("Error sending sign msg: %v", err)
			return s.sendRejectBatch(batch, err)
		}

	// The previously prepared batch has been executed and we can finalize
	// it by opening the channel and persisting the account and order diffs.
	case *poolrpc.ServerAuctionMessage_Finalize:
		rpcLog.Tracef("Received finalize msg from server, batch_id=%x: %v",
			msg.Finalize.BatchId, spew.Sdump(msg))

		rpcLog.Infof("Received FinalizeMsg for batch=%x",
			msg.Finalize.BatchId)

		// Before finalizing the batch, we want to know what accounts
		// were involved so we can start watching them again. Query the
		// pending batch now as BatchFinalize below will set it to nil.
		batch := s.orderManager.PendingBatch()

		var batchID order.BatchID
		copy(batchID[:], msg.Finalize.BatchId)
		err := s.orderManager.BatchFinalize(batchID)
		if err != nil {
			return fmt.Errorf("error finalizing batch: %v", err)
		}

		// We've successfully processed the finalize message, let's
		// store an event for this for all orders that were involved on
		// our side.
		if err := s.server.db.StoreBatchEvents(
			batch, order.MatchStateFinalized,
			poolrpc.MatchRejectReason_NONE,
		); err != nil {
			rpcLog.Errorf("Unable to store order events: %v", err)
		}

		// Accounts that were updated in the batch need to start new
		// confirmation watchers, now that we expect a batch TX to be
		// published.
		matchedAccounts := make(
			[]*btcec.PublicKey, len(batch.AccountDiffs),
		)
		for idx, acct := range batch.AccountDiffs {
			matchedAccounts[idx] = acct.AccountKey
		}
		return s.accountManager.WatchMatchedAccounts(
			context.Background(), matchedAccounts,
		)

	default:
		return fmt.Errorf("unknown server message: %v", msg)
	}

	return nil
}

func (s *rpcServer) QuoteAccount(ctx context.Context,
	req *poolrpc.QuoteAccountRequest) (*poolrpc.QuoteAccountResponse, error) {

	// Determine the desired transaction fee.
	confTarget := req.GetConfTarget()
	if confTarget < 1 {
		return nil, fmt.Errorf("confirmation target must be " +
			"greater than 0")
	}

	feeRate, totalFee, err := s.accountManager.QuoteAccount(
		ctx, btcutil.Amount(req.AccountValue), confTarget,
	)
	if err != nil {
		return nil, err
	}

	return &poolrpc.QuoteAccountResponse{
		MinerFeeRateSatPerKw: uint64(feeRate),
		MinerFeeTotal:        uint64(totalFee),
	}, nil
}

func (s *rpcServer) InitAccount(ctx context.Context,
	req *poolrpc.InitAccountRequest) (*poolrpc.Account, error) {

	bestHeight := atomic.LoadUint32(&s.bestHeight)

	// Determine the desired expiration value, can be relative or absolute.
	var expiryHeight uint32
	switch {
	case req.GetAbsoluteHeight() != 0:
		expiryHeight = req.GetAbsoluteHeight()

	case req.GetRelativeHeight() != 0:
		expiryHeight = req.GetRelativeHeight() + bestHeight

	default:
		return nil, fmt.Errorf("either relative or absolute height " +
			"must be specified")
	}

	// Determine the desired transaction fee.
	confTarget := req.GetConfTarget()
	if confTarget < 1 {
		return nil, fmt.Errorf("confirmation target must be " +
			"greater than 0")
	}

	acct, err := s.accountManager.InitAccount(
		ctx, btcutil.Amount(req.AccountValue), expiryHeight,
		bestHeight, confTarget,
	)
	if err != nil {
		return nil, err
	}

	return MarshallAccount(acct)
}

func (s *rpcServer) ListAccounts(ctx context.Context,
	req *poolrpc.ListAccountsRequest) (*poolrpc.ListAccountsResponse, error) {

	accounts, err := s.server.db.Accounts()
	if err != nil {
		return nil, err
	}

	rpcAccounts := make([]*poolrpc.Account, 0, len(accounts))
	for _, acct := range accounts {
		// Filter out inactive accounts if requested by the user.
		if req.ActiveOnly && !acct.State.IsActive() {
			continue
		}

		rpcAccount, err := MarshallAccount(acct)
		if err != nil {
			return nil, err
		}
		rpcAccounts = append(rpcAccounts, rpcAccount)
	}

	// For each account, we'll need to compute the available balance, which
	// requires us to sum up all the debits from outstanding orders.
	orders, err := s.server.db.GetOrders()
	if err != nil {
		return nil, err
	}

	// Get the current fee schedule so we can compute the worst-case
	// account debit assuming all our standing orders were matched.
	auctionTerms, err := s.auctioneer.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to query auctioneer terms: %v",
			err)
	}

	// For each active account, consume the worst-case account delta if the
	// order were to be matched.
	accountDebits := make(map[[33]byte]btcutil.Amount)
	auctionFeeSchedule := auctionTerms.FeeSchedule()
	for _, acct := range accounts {
		var (
			debitAmt btcutil.Amount
			acctKey  [33]byte
		)

		copy(
			acctKey[:],
			acct.TraderKey.PubKey.SerializeCompressed(),
		)

		// We'll make sure to accumulate a distinct sum for each
		// outstanding account the user has.
		for _, o := range orders {
			if o.Details().AcctKey != acctKey {
				continue
			}

			debitAmt += o.ReservedValue(auctionFeeSchedule)
		}

		accountDebits[acctKey] = debitAmt
	}

	// Finally, we'll populate the available balance value for each of the
	// existing accounts.
	for _, rpcAccount := range rpcAccounts {
		var acctKey [33]byte
		copy(acctKey[:], rpcAccount.TraderKey)

		accountDebit := accountDebits[acctKey]
		availableBalance := rpcAccount.Value - uint64(accountDebit)

		rpcAccount.AvailableBalance = availableBalance
	}

	return &poolrpc.ListAccountsResponse{
		Accounts: rpcAccounts,
	}, nil
}

// MarshallAccount returns the RPC representation of an account.
func MarshallAccount(a *account.Account) (*poolrpc.Account, error) {
	var rpcState poolrpc.AccountState
	switch a.State {
	case account.StateInitiated, account.StatePendingOpen:
		rpcState = poolrpc.AccountState_PENDING_OPEN

	case account.StatePendingUpdate:
		rpcState = poolrpc.AccountState_PENDING_UPDATE

	case account.StateOpen:
		rpcState = poolrpc.AccountState_OPEN

	case account.StateExpired:
		rpcState = poolrpc.AccountState_EXPIRED

	case account.StatePendingClosed:
		rpcState = poolrpc.AccountState_PENDING_CLOSED

	case account.StateClosed:
		rpcState = poolrpc.AccountState_CLOSED

	case account.StateCanceledAfterRecovery:
		rpcState = poolrpc.AccountState_RECOVERY_FAILED

	case account.StatePendingBatch:
		rpcState = poolrpc.AccountState_PENDING_BATCH

	default:
		return nil, fmt.Errorf("unknown state %v", a.State)
	}

	// The latest transaction is only known after the account has been
	// funded.
	var latestTxHash chainhash.Hash
	if a.LatestTx != nil {
		latestTxHash = a.LatestTx.TxHash()
	}

	return &poolrpc.Account{
		TraderKey: a.TraderKey.PubKey.SerializeCompressed(),
		Outpoint: &poolrpc.OutPoint{
			Txid:        a.OutPoint.Hash[:],
			OutputIndex: a.OutPoint.Index,
		},
		Value:            uint64(a.Value),
		ExpirationHeight: a.Expiry,
		State:            rpcState,
		LatestTxid:       latestTxHash[:],
	}, nil
}

// DepositAccount handles a trader's request to deposit funds into the specified
// account by spending the specified inputs.
func (s *rpcServer) DepositAccount(ctx context.Context,
	req *poolrpc.DepositAccountRequest) (*poolrpc.DepositAccountResponse, error) {

	rpcLog.Infof("Depositing %v into acct=%x",
		btcutil.Amount(req.AmountSat), req.TraderKey)

	// Ensure the trader key is well formed.
	traderKey, err := btcec.ParsePubKey(req.TraderKey, btcec.S256())
	if err != nil {
		return nil, err
	}

	// Enforce a minimum fee rate of 253 sat/kw.
	feeRate := chainfee.SatPerKWeight(req.FeeRateSatPerKw)
	if feeRate < chainfee.FeePerKwFloor {
		return nil, fmt.Errorf("fee rate of %d sat/kw is too low, "+
			"minimum is %d sat/kw", feeRate, chainfee.FeePerKwFloor)
	}

	// Proceed to process the deposit and map its response to the RPC's
	// response.
	modifiedAccount, tx, err := s.accountManager.DepositAccount(
		ctx, traderKey, btcutil.Amount(req.AmountSat), feeRate,
		atomic.LoadUint32(&s.bestHeight),
	)
	if err != nil {
		return nil, err
	}

	rpcModifiedAccount, err := MarshallAccount(modifiedAccount)
	if err != nil {
		return nil, err
	}
	txHash := tx.TxHash()

	return &poolrpc.DepositAccountResponse{
		Account:     rpcModifiedAccount,
		DepositTxid: txHash[:],
	}, nil
}

// WithdrawAccount handles a trader's request to withdraw funds from the
// specified account by spending the current account output to the specified
// outputs.
func (s *rpcServer) WithdrawAccount(ctx context.Context,
	req *poolrpc.WithdrawAccountRequest) (*poolrpc.WithdrawAccountResponse, error) {

	rpcLog.Infof("Withdrawing from acct=%x", req.TraderKey)

	// Ensure the trader key is well formed.
	traderKey, err := btcec.ParsePubKey(req.TraderKey, btcec.S256())
	if err != nil {
		return nil, err
	}

	// Ensure the outputs we'll withdraw to are well formed.
	if len(req.Outputs) == 0 {
		return nil, errors.New("missing outputs for withdrawal")
	}
	outputs, err := s.parseRPCOutputs(req.Outputs)
	if err != nil {
		return nil, err
	}

	// Enforce a minimum fee rate of 253 sat/kw.
	feeRate := chainfee.SatPerKWeight(req.FeeRateSatPerKw)
	if feeRate < chainfee.FeePerKwFloor {
		return nil, fmt.Errorf("fee rate of %d sat/kw is too low, "+
			"minimum is %d sat/kw", feeRate, chainfee.FeePerKwFloor)
	}

	// Proceed to process the withdrawal and map its response to the RPC's
	// response.
	modifiedAccount, tx, err := s.accountManager.WithdrawAccount(
		ctx, traderKey, outputs, feeRate,
		atomic.LoadUint32(&s.bestHeight),
	)
	if err != nil {
		return nil, err
	}

	rpcModifiedAccount, err := MarshallAccount(modifiedAccount)
	if err != nil {
		return nil, err
	}
	txHash := tx.TxHash()

	return &poolrpc.WithdrawAccountResponse{
		Account:      rpcModifiedAccount,
		WithdrawTxid: txHash[:],
	}, nil
}

// BumpAccountFee attempts to bump the fee of an account's transaction through
// child-pays-for-parent (CPFP). Since the CPFP is performed through the backing
// lnd node, the account transaction must contain an output under its control
// for a successful bump. If a CPFP has already been performed for an account,
// and this RPC is invoked again, then a replacing transaction (RBF) of the
// child will be broadcast.
func (s *rpcServer) BumpAccountFee(ctx context.Context,
	req *poolrpc.BumpAccountFeeRequest) (*poolrpc.BumpAccountFeeResponse, error) {

	traderKey, err := btcec.ParsePubKey(req.TraderKey, btcec.S256())
	if err != nil {
		return nil, err
	}
	feeRate := chainfee.SatPerKWeight(req.FeeRateSatPerKw)

	err = s.accountManager.BumpAccountFee(ctx, traderKey, feeRate)
	if err != nil {
		return nil, err
	}

	return &poolrpc.BumpAccountFeeResponse{}, nil
}

// CloseAccount handles a trader's request to close the specified account.
func (s *rpcServer) CloseAccount(ctx context.Context,
	req *poolrpc.CloseAccountRequest) (*poolrpc.CloseAccountResponse, error) {

	rpcLog.Infof("Closing acct=%x", req.TraderKey)

	traderKey, err := btcec.ParsePubKey(req.TraderKey, btcec.S256())
	if err != nil {
		return nil, err
	}

	// Ensure a valid fee expression was provided.
	var feeExpr account.FeeExpr
	switch dest := req.FundsDestination.(type) {
	// The fee is expressed as a combination of an output along with a fee
	// rate.
	case *poolrpc.CloseAccountRequest_OutputWithFee:
		// Parse the output script if one was provided and ensure it's
		// valid.
		var pkScript []byte
		if dest.OutputWithFee.Address != "" {
			var err error
			pkScript, err = s.parseOutputScript(
				dest.OutputWithFee.Address,
			)
			if err != nil {
				return nil, err
			}
		}

		// Parse the provided fee rate.
		var feeRate chainfee.SatPerKWeight
		switch feeExpr := dest.OutputWithFee.Fees.(type) {
		case *poolrpc.OutputWithFee_ConfTarget:
			var err error
			feeRate, err = s.lndServices.WalletKit.EstimateFee(
				ctx, int32(feeExpr.ConfTarget),
			)
			if err != nil {
				return nil, err
			}

		case *poolrpc.OutputWithFee_FeeRateSatPerKw:
			feeRate = chainfee.SatPerKWeight(feeExpr.FeeRateSatPerKw)
		}

		// Enforce a minimum fee rate of 253 sat/kw.
		if feeRate < chainfee.FeePerKwFloor {
			return nil, fmt.Errorf("fee rate of %v is too low, "+
				"minimum is %v", feeRate, chainfee.FeePerKwFloor)
		}

		feeExpr = &account.OutputWithFee{
			PkScript: pkScript,
			FeeRate:  feeRate,
		}

	// The fee is expressed as implicitly defined by the total output value
	// of the provided outputs.
	case *poolrpc.CloseAccountRequest_Outputs:
		if len(dest.Outputs.Outputs) == 0 {
			return nil, errors.New("no outputs provided")
		}

		outputs, err := s.parseRPCOutputs(dest.Outputs.Outputs)
		if err != nil {
			return nil, err
		}

		feeExpr = account.OutputsWithImplicitFee(outputs)

	case nil:
		return nil, errors.New("a funds destination must be specified")
	}

	dbOrders, err := s.server.db.GetOrders()
	if err != nil {
		return nil, err
	}

	var openNonces []order.Nonce
	for _, dbOrder := range dbOrders {
		orderDetails := dbOrder.Details()
		nonce := orderDetails.Nonce()

		// There's no need to query for an order in a terminal state
		// from our PoV as it can't transition back to active.
		if dbOrder.Details().State.Archived() {
			continue
		}

		// To ensure we have the latest order state, we'll consult with
		// the auctioneer's state.
		orderStateResp, err := s.auctioneer.OrderState(ctx, nonce)
		if err != nil {
			return nil, err
		}
		orderState, err := rpcOrderStateToDBState(orderStateResp.State)
		if err != nil {
			return nil, err
		}

		// If the order isn't in the base state, then we'll skip it as
		// it isn't considered an "active" order.
		if orderState.Archived() {
			continue
		}

		if bytes.Equal(orderDetails.AcctKey[:], req.TraderKey) {
			openNonces = append(openNonces, orderDetails.Nonce())
		}
	}

	// We don't allow an account to be closed if it has open orders so they
	// don't dangle in the order book on the server's side.
	if len(openNonces) > 0 {
		return nil, fmt.Errorf("acct=%x has open orders, cancel them "+
			"before closing: %v", req.TraderKey, openNonces)
	}

	// Proceed to close the requested account with the parsed fee
	// expression.
	closeTx, err := s.accountManager.CloseAccount(
		ctx, traderKey, feeExpr, atomic.LoadUint32(&s.bestHeight),
	)
	if err != nil {
		return nil, err
	}
	closeTxHash := closeTx.TxHash()

	return &poolrpc.CloseAccountResponse{
		CloseTxid: closeTxHash[:],
	}, nil
}

// parseOutputScript parses the output script from the string representation of
// an on-chain address and ensures it's valid for the current network.
func (s *rpcServer) parseOutputScript(addrStr string) ([]byte, error) {
	addr, err := btcutil.DecodeAddress(addrStr, s.lndServices.ChainParams)
	if err != nil {
		return nil, err
	}
	if !addr.IsForNet(s.lndServices.ChainParams) {
		return nil, fmt.Errorf("invalid address %v for %v "+
			"network", addr.String(),
			s.lndServices.ChainParams.Name)
	}
	return txscript.PayToAddrScript(addr)
}

// parseRPCOutputs maps []*poolrpc.Output -> []*wire.TxOut.
func (s *rpcServer) parseRPCOutputs(outputs []*poolrpc.Output) ([]*wire.TxOut,
	error) {

	res := make([]*wire.TxOut, 0, len(outputs))
	for _, output := range outputs {
		outputScript, err := s.parseOutputScript(output.Address)
		if err != nil {
			return nil, err
		}
		res = append(res, &wire.TxOut{
			Value:    int64(output.ValueSat),
			PkScript: outputScript,
		})
	}

	return res, nil
}

func (s *rpcServer) RecoverAccounts(ctx context.Context,
	_ *poolrpc.RecoverAccountsRequest) (*poolrpc.RecoverAccountsResponse,
	error) {

	log.Infof("Attempting to recover accounts...")

	s.recoveryMutex.Lock()
	if s.recoveryPending {
		defer s.recoveryMutex.Unlock()
		return nil, fmt.Errorf("recovery already in progress")
	}
	s.recoveryPending = true
	s.recoveryMutex.Unlock()

	// Prepare the keys we are going to try. Possibly not all of them will
	// be used.
	acctKeys, err := account.GenerateRecoveryKeys(
		ctx, s.lndServices.WalletKit,
	)
	if err != nil {
		return nil, fmt.Errorf("error generating keys: %v", err)
	}

	// The auctioneer client will try to recover accounts for these keys as
	// long as the auctioneer is able to find them in its database. If a
	// certain number of keys result in an "account not found" error, the
	// client will stop trying.
	recoveredAccounts, err := s.auctioneer.RecoverAccounts(ctx, acctKeys)
	if err != nil {
		return nil, fmt.Errorf("error performing recovery: %v", err)
	}

	// Store the recovered accounts now and start watching them. If anything
	// went wrong during recovery before, no account is stored yet. This is
	// nice since it allows us to try recovery multiple times until it
	// actually works.
	numRecovered := len(recoveredAccounts)
	for _, acct := range recoveredAccounts {
		err = s.accountManager.RecoverAccount(ctx, acct)
		if err != nil {
			// If something goes wrong for one account we still want
			// to continue with the others.
			numRecovered--
			rpcLog.Errorf("error storing recovered account: %v", err)
		}
	}

	s.recoveryMutex.Lock()
	s.recoveryPending = false
	s.recoveryMutex.Unlock()

	return &poolrpc.RecoverAccountsResponse{
		NumRecoveredAccounts: uint32(numRecovered),
	}, nil
}

// SubmitOrder assembles all the information that is required to submit an order
// from the trader's lnd node, signs it and then sends the order to the server
// to be included in the auctioneer's order book.
func (s *rpcServer) SubmitOrder(ctx context.Context,
	req *poolrpc.SubmitOrderRequest) (*poolrpc.SubmitOrderResponse, error) {

	var o order.Order
	switch requestOrder := req.Details.(type) {
	case *poolrpc.SubmitOrderRequest_Ask:
		a := requestOrder.Ask
		kit, err := order.ParseRPCOrder(
			a.Version, a.LeaseDurationBlocks, a.Details,
		)
		if err != nil {
			return nil, err
		}

		o = &order.Ask{
			Kit: *kit,
		}

	case *poolrpc.SubmitOrderRequest_Bid:
		b := requestOrder.Bid
		kit, err := order.ParseRPCOrder(
			b.Version, b.LeaseDurationBlocks, b.Details,
		)
		if err != nil {
			return nil, err
		}
		nodeTier, err := unmarshallNodeTier(b.MinNodeTier)
		if err != nil {
			return nil, err
		}

		o = &order.Bid{
			Kit:         *kit,
			MinNodeTier: nodeTier,
		}

	default:
		return nil, fmt.Errorf("invalid order request")
	}

	// Now that we now how large the order is, ensure that if it's a
	// wumbo-sized order, then the backing lnd node is advertising wumbo
	// support.
	if o.Details().Amt > lnd.MaxBtcFundingAmount && !s.wumboSupported {
		return nil, fmt.Errorf("order of %v is wumbo sized, but "+
			"lnd node isn't signalling wumbo", o.Details().Amt)
	}

	// Verify that the account exists.
	acctKey, err := btcec.ParsePubKey(
		o.Details().AcctKey[:], btcec.S256(),
	)
	if err != nil {
		return nil, err
	}
	acct, err := s.server.db.Account(acctKey)
	if err != nil {
		return nil, fmt.Errorf("cannot accept order: %v", err)
	}

	// We'll only allow orders for accounts that present in an open state,
	// or have a pending update or batch. On the server-side if we have a
	// pending update we won't be matched, but this lets us place our orders
	// early so we can join the earliest available batch.
	switch acct.State {
	case account.StateOpen, account.StatePendingUpdate,
		account.StatePendingBatch:

		break

	default:
		return nil, fmt.Errorf("acct=%x is in state %v, cannot "+
			"make order", o.Details().AcctKey[:], acct.State)
	}

	// We also need to know the current maximum order duration.
	auctionTerms, err := s.auctioneer.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not query auctioneer terms: %v",
			err)
	}

	// If the market isn't currently accepting orders for this particular
	// lease duration, then we'll exit here as the order will be rejected.
	leaseDuration := o.Details().LeaseDuration
	if _, ok := auctionTerms.LeaseDurations[leaseDuration]; !ok {
		return nil, fmt.Errorf("invalid channel lease duration %v "+
			"blocks, active durations are: %v",
			leaseDuration, auctionTerms.LeaseDurations)
	}

	// Collect all the order data and sign it before sending it to the
	// auction server.
	serverParams, err := s.orderManager.PrepareOrder(ctx, o, acct, auctionTerms)
	if err != nil {
		return nil, err
	}

	// Send the order to the server. If this fails, then the order is
	// certain to never get into the order book. We don't need to keep it
	// around in that case.
	err = s.auctioneer.SubmitOrder(ctx, o, serverParams)
	if err != nil {
		// The server rejected the order. We keep it around for now,
		// failed orders can be filtered by specifying --active_only
		// when listing orders.
		err2 := s.server.db.UpdateOrder(
			o.Nonce(), order.StateModifier(order.StateFailed),
		)
		if err2 != nil {
			rpcLog.Errorf("Could not update failed order: %v", err2)
		}

		// If there was something wrong with the information the user
		// provided, then return this as a nice string instead of an
		// error type.
		if userErr, ok := err.(*order.UserError); ok {
			rpcLog.Warnf("Invalid order details: %v", userErr)

			return &poolrpc.SubmitOrderResponse{
				Details: &poolrpc.SubmitOrderResponse_InvalidOrder{
					InvalidOrder: userErr.Details,
				},
			}, nil
		}

		// Any other error we return normally as a gRPC status level
		// error.
		return nil, fmt.Errorf("error submitting order to auctioneer: "+
			"%v", err)
	}

	rpcLog.Infof("New order submitted: nonce=%v, type=%v", o.Nonce(), o.Type())

	// ServerOrder is accepted.
	orderNonce := o.Nonce()
	return &poolrpc.SubmitOrderResponse{
		Details: &poolrpc.SubmitOrderResponse_AcceptedOrderNonce{
			AcceptedOrderNonce: orderNonce[:],
		},
	}, nil
}

// ListOrders returns a list of all orders that is currently known to the trader
// client's local store. The state of each order is queried on the auction
// server and returned as well.
func (s *rpcServer) ListOrders(ctx context.Context,
	req *poolrpc.ListOrdersRequest) (*poolrpc.ListOrdersResponse, error) {

	// Because we made sure every order has a creation timestamp in the
	// events bucket, we can now access the orders in a sorted way if we
	// first grab all creation events. This might be a bit less efficient
	// because we first need to query the events bucket. But this can easily
	// be solved by adding a write-through cache for the events, then that
	// bucket serves both as an unique order sequence as well as the event
	// source and is really fast to access.
	creationEvents, err := s.server.db.AllEvents(event.TypeOrderCreated)
	if err != nil {
		return nil, fmt.Errorf("error querying order creation events: "+
			"%v", err)
	}

	// If we cannot query the auctioneer, we just use an empty fee
	// schedule, as it is only used for calculating the reserved value.
	var feeSchedule terms.FeeSchedule = terms.NewLinearFeeSchedule(0, 0)
	auctioneerTerms, err := s.auctioneer.Terms(ctx)
	if err != nil {
		log.Warnf("unable to query auctioneer terms: %v", err)
	} else {
		feeSchedule = auctioneerTerms.FeeSchedule()
	}

	// The RPC is split by order type so we have to separate them now.
	asks := make([]*poolrpc.Ask, 0, len(creationEvents))
	bids := make([]*poolrpc.Bid, 0, len(creationEvents))
	for _, evt := range creationEvents {
		orderCreateEvent, ok := evt.(*clientdb.CreatedEvent)
		if !ok {
			return nil, fmt.Errorf("invalid order create event: %v",
				evt)
		}

		dbOrder, err := s.server.db.GetOrder(orderCreateEvent.Nonce())
		if err != nil {
			return nil, err
		}
		nonce := dbOrder.Nonce()
		dbDetails := dbOrder.Details()

		// Skip order if it is archived and only active orders are
		// requested.
		if dbDetails.State.Archived() && req.ActiveOnly {
			continue
		}

		orderState, err := DBOrderStateToRPCState(dbDetails.State)
		if err != nil {
			return nil, err
		}

		var rpcEvents []*poolrpc.OrderEvent
		if req.Verbose {
			dbEvents, err := s.server.db.GetOrderEvents(nonce)
			if err != nil {
				return nil, err
			}

			rpcEvents, err = dbEventsToRPCEvents(dbEvents)
			if err != nil {
				return nil, err
			}
		}

		details := &poolrpc.Order{
			TraderKey: dbDetails.AcctKey[:],
			RateFixed: dbDetails.FixedRate,
			Amt:       uint64(dbDetails.Amt),
			MaxBatchFeeRateSatPerKw: uint64(
				dbDetails.MaxBatchFeeRate,
			),
			OrderNonce:       nonce[:],
			State:            orderState,
			Units:            uint32(dbDetails.Units),
			UnitsUnfulfilled: uint32(dbDetails.UnitsUnfulfilled),
			ReservedValueSat: uint64(
				dbOrder.ReservedValue(feeSchedule),
			),
			CreationTimestampNs: uint64(evt.Timestamp().UnixNano()),
			Events:              rpcEvents,
			MinUnitsMatch:       uint32(dbOrder.Details().MinUnitsMatch),
		}

		switch o := dbOrder.(type) {
		case *order.Ask:
			rpcAsk := &poolrpc.Ask{
				Details:             details,
				LeaseDurationBlocks: dbDetails.LeaseDuration,
				Version:             uint32(o.Version),
			}
			asks = append(asks, rpcAsk)

		case *order.Bid:
			nodeTier, err := auctioneer.MarshallNodeTier(o.MinNodeTier)
			if err != nil {
				return nil, err
			}

			rpcBid := &poolrpc.Bid{
				Details:             details,
				LeaseDurationBlocks: dbDetails.LeaseDuration,
				Version:             uint32(o.Version),
				MinNodeTier:         nodeTier,
			}
			bids = append(bids, rpcBid)

		default:
			return nil, fmt.Errorf("unknown order type: %v",
				o)
		}
	}
	return &poolrpc.ListOrdersResponse{
		Asks: asks,
		Bids: bids,
	}, nil
}

// CancelOrder cancels the order on the server and updates the state of the
// local order accordingly.
func (s *rpcServer) CancelOrder(ctx context.Context,
	req *poolrpc.CancelOrderRequest) (*poolrpc.CancelOrderResponse, error) {

	var nonce order.Nonce
	copy(nonce[:], req.OrderNonce)

	o, err := s.server.db.GetOrder(nonce)
	if err != nil {
		return nil, err
	}

	switch o.Details().State {
	// The order has already been canceled, just return now.
	case order.StateCanceled:
		return &poolrpc.CancelOrderResponse{}, nil

	// The order has reached a terminal state so there's no point in
	// canceling it.
	case order.StateExecuted, order.StateExpired, order.StateFailed:
		return nil, fmt.Errorf("unable to cancel order in terminal "+
			"state %v", o.Details().State)
	default:
		break
	}

	rpcLog.Infof("Cancelling order_nonce=%v", nonce)

	// We cancel an order in two phases. First, we'll cancel the order on
	// the server-side.
	err = s.auctioneer.CancelOrder(ctx, o.Details().Preimage)
	if err != nil {
		return nil, err
	}

	// Now that we've cancelled things on the server-side, we'll update our
	// local state to reflect this change in the order. If we crash here,
	// then we'll sync up the order state once we restart again.
	err = s.server.db.UpdateOrder(
		nonce, order.StateModifier(order.StateCanceled),
	)
	if err != nil {
		return nil, err
	}

	return &poolrpc.CancelOrderResponse{}, nil
}

// sendRejectBatch sends a reject message to the server with the properly
// decoded reason code and the full reason message as a string.
func (s *rpcServer) sendRejectBatch(batch *order.Batch, failure error) error {
	// As we're rejecting this batch, we'll now cancel all funding shims
	// that we may have registered since we may be matched with a distinct
	// set of channels if this batch is repeated.
	err := funding.CancelPendingFundingShims(
		batch.MatchedOrders, s.lndClient,
		func(o order.Nonce) (order.Order, error) {
			return s.server.db.GetOrder(o)
		},
	)
	if err != nil {
		return err
	}

	msg := &poolrpc.ClientAuctionMessage_Reject{
		Reject: &poolrpc.OrderMatchReject{
			BatchId: batch.ID[:],
			Reason:  failure.Error(),
		},
	}

	// Attach the status code to the message to give a bit more context.
	var partialReject *funding.MatchRejectErr
	switch {
	case errors.Is(failure, order.ErrVersionMismatch):
		msg.Reject.ReasonCode = poolrpc.OrderMatchReject_BATCH_VERSION_MISMATCH

		// Track this reject by adding an event to our orders.
		err := s.server.db.StoreBatchEvents(
			batch, order.MatchStateRejected,
			poolrpc.MatchRejectReason_BATCH_VERSION_MISMATCH,
		)
		if err != nil {
			rpcLog.Errorf("Could not store match event: %v", err)
		}

	case errors.Is(failure, order.ErrMismatchErr):
		msg.Reject.ReasonCode = poolrpc.OrderMatchReject_SERVER_MISBEHAVIOR

		// Track this reject by adding an event to our orders.
		err := s.server.db.StoreBatchEvents(
			batch, order.MatchStateRejected,
			poolrpc.MatchRejectReason_SERVER_MISBEHAVIOR,
		)
		if err != nil {
			rpcLog.Errorf("Could not store match event: %v", err)
		}

	case errors.As(failure, &partialReject):
		msg.Reject.ReasonCode = poolrpc.OrderMatchReject_PARTIAL_REJECT
		msg.Reject.RejectedOrders = make(map[string]*poolrpc.OrderReject)
		for nonce, reject := range partialReject.RejectedOrders {
			msg.Reject.RejectedOrders[nonce.String()] = reject
		}

		// Track this reject by adding an event to our orders with the
		// specific reject code for each of rejected orders.
		err := s.server.db.StoreBatchPartialRejectEvents(
			batch, partialReject.RejectedOrders,
		)
		if err != nil {
			rpcLog.Errorf("Could not store match event: %v", err)
		}

	default:
		msg.Reject.ReasonCode = poolrpc.OrderMatchReject_UNKNOWN
	}

	rpcLog.Infof("Sending batch rejection message for batch %x with "+
		"code %v and message: %v", batch.ID, msg.Reject.ReasonCode,
		failure)

	// Send the message to the server. If a new error happens we return that
	// one because we know the causing error has at least been logged at
	// some point before.
	err = s.auctioneer.SendAuctionMessage(&poolrpc.ClientAuctionMessage{
		Msg: msg,
	})
	if err != nil {
		return fmt.Errorf("error sending reject message: %v", err)
	}

	// We have handled the batch failure and informed the auctioneer. We
	// have done our job so no need to return an error.
	return nil
}

// sendAcceptBatch sends an accept message to the server with the list of order
// nonces that we accept in the batch.
func (s *rpcServer) sendAcceptBatch(batch *order.Batch) error {
	rpcLog.Infof("Accepting batch=%x", batch.ID[:])

	// Let's store an event for each order in the batch that we did accept
	// the batch.
	if err := s.server.db.StoreBatchEvents(
		batch, order.MatchStateAccepted, poolrpc.MatchRejectReason_NONE,
	); err != nil {
		rpcLog.Errorf("Unable to store order events: %v", err)
	}

	// Send the message to the server.
	return s.auctioneer.SendAuctionMessage(&poolrpc.ClientAuctionMessage{
		Msg: &poolrpc.ClientAuctionMessage_Accept{
			Accept: &poolrpc.OrderMatchAccept{
				BatchId: batch.ID[:],
			},
		},
	})
}

// GetLsatTokens returns all tokens that are contained in the LSAT token store.
func (s *rpcServer) GetLsatTokens(_ context.Context,
	_ *poolrpc.TokensRequest) (*poolrpc.TokensResponse, error) {

	log.Infof("Get LSAT tokens request received")

	tokens, err := s.server.lsatStore.AllTokens()
	if err != nil {
		return nil, err
	}

	rpcTokens := make([]*poolrpc.LsatToken, len(tokens))
	idx := 0
	for key, token := range tokens {
		macBytes, err := token.BaseMacaroon().MarshalBinary()
		if err != nil {
			return nil, err
		}
		rpcTokens[idx] = &poolrpc.LsatToken{
			BaseMacaroon:       macBytes,
			PaymentHash:        token.PaymentHash[:],
			PaymentPreimage:    token.Preimage[:],
			AmountPaidMsat:     int64(token.AmountPaid),
			RoutingFeePaidMsat: int64(token.RoutingFeePaid),
			TimeCreated:        token.TimeCreated.Unix(),
			Expired:            !token.IsValid(),
			StorageName:        key,
		}
		idx++
	}

	return &poolrpc.TokensResponse{Tokens: rpcTokens}, nil
}

// sendSignBatch sends a sign message to the server with the witness stacks of
// all accounts that are involved in the batch.
func (s *rpcServer) sendSignBatch(batch *order.Batch, sigs order.BatchSignature,
	chanInfos map[wire.OutPoint]*chaninfo.ChannelInfo) error {

	// Prepare the list of witness stacks and channel infos and send them to
	// the server.
	rpcSigs := make(map[string][]byte, len(sigs))
	for acctKey, sig := range sigs {
		key := hex.EncodeToString(acctKey[:])
		rpcSigs[key] = sig.Serialize()
	}

	rpcChannelInfos := make(map[string]*poolrpc.ChannelInfo, len(chanInfos))
	for chanPoint, chanInfo := range chanInfos {
		var channelType poolrpc.ChannelType
		switch chanInfo.Version {
		case chanbackup.TweaklessCommitVersion:
			channelType = poolrpc.ChannelType_TWEAKLESS
		case chanbackup.AnchorsCommitVersion:
			channelType = poolrpc.ChannelType_ANCHORS
		default:
			return fmt.Errorf("unknown channel type: %v",
				chanInfo.Version)
		}
		rpcChannelInfos[chanPoint.String()] = &poolrpc.ChannelInfo{
			Type: channelType,
			LocalNodeKey: chanInfo.LocalNodeKey.
				SerializeCompressed(),
			RemoteNodeKey: chanInfo.RemoteNodeKey.
				SerializeCompressed(),
			LocalPaymentBasePoint: chanInfo.LocalPaymentBasePoint.
				SerializeCompressed(),
			RemotePaymentBasePoint: chanInfo.RemotePaymentBasePoint.
				SerializeCompressed(),
		}
	}

	rpcLog.Infof("Sending OrderMatchSign for batch %x", batch.ID[:])

	// We've successfully processed the sign message, let's store an event
	// for this for all orders that were involved on our side.
	if err := s.server.db.StoreBatchEvents(
		batch, order.MatchStateSigned, poolrpc.MatchRejectReason_NONE,
	); err != nil {
		rpcLog.Errorf("Unable to store order events: %v", err)
	}

	return s.auctioneer.SendAuctionMessage(&poolrpc.ClientAuctionMessage{
		Msg: &poolrpc.ClientAuctionMessage_Sign{
			Sign: &poolrpc.OrderMatchSign{
				BatchId:      batch.ID[:],
				AccountSigs:  rpcSigs,
				ChannelInfos: rpcChannelInfos,
			},
		},
	})
}

// AuctionFee returns the current fee rate charged for matched orders within
// the auction.
func (s *rpcServer) AuctionFee(ctx context.Context,
	_ *poolrpc.AuctionFeeRequest) (*poolrpc.AuctionFeeResponse, error) {

	auctionTerms, err := s.auctioneer.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to query auctioneer terms: %v",
			err)
	}

	// TODO(roasbeef): accept the amt of order instead?
	return &poolrpc.AuctionFeeResponse{
		ExecutionFee: &poolrpc.ExecutionFee{
			BaseFee: uint64(auctionTerms.OrderExecBaseFee),
			FeeRate: uint64(auctionTerms.OrderExecFeeRate),
		},
	}, nil
}

// Leases returns the list of channels that were either purchased or sold by the
// trader within the auction.
func (s *rpcServer) Leases(ctx context.Context,
	req *poolrpc.LeasesRequest) (*poolrpc.LeasesResponse, error) {

	// We'll start by parsing the list of accounts we should retrieve lease
	// for. If none are specified, leases for all accounts will be returned.
	accounts := make(map[[33]byte]struct{}, len(req.Accounts))
	for _, rawAccountKey := range req.Accounts {
		_, err := btcec.ParsePubKey(rawAccountKey, btcec.S256())
		if err != nil {
			return nil, fmt.Errorf("invalid account key: %v", err)
		}

		var accountKey [33]byte
		copy(accountKey[:], rawAccountKey)
		accounts[accountKey] = struct{}{}
	}

	// We'll then retrieve the list of specified batches. If none are
	// specified, we'll retrieve all of them.
	var batches []*clientdb.LocalBatchSnapshot
	if len(req.BatchIds) == 0 {
		var err error
		batches, err = s.server.db.GetLocalBatchSnapshots()
		if err != nil {
			return nil, err
		}
	} else {
		for _, rawBatchID := range req.BatchIds {
			batchKey, err := btcec.ParsePubKey(
				rawBatchID, btcec.S256(),
			)
			if err != nil {
				return nil, fmt.Errorf("invalid batch id: %v",
					err)
			}

			batchID := order.NewBatchID(batchKey)
			batch, err := s.server.db.GetLocalBatchSnapshot(batchID)
			if err != nil {
				return nil, err
			}
			batches = append(batches, batch)
		}
	}

	// As the true lease expiry of a channel is only known after tit has
	// been confirmed, we'll assemble a map from the outpoint of a channel
	// to the thaw_height which is actually the lease expiry height.
	openChans, err := s.lndClient.ListChannels(
		context.Background(), &lnrpc.ListChannelsRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch set of open "+
			"channels: %v", err)
	}

	// We'll map the outpoint string representation of a channel to the
	// actual lease expiry height which we'll use to partially populate our
	// response.
	chanLeaseExpiries := make(map[string]uint32)
	for _, channel := range openChans.Channels {
		if channel.ThawHeight == 0 {
			continue
		}

		// The thaw height is relative for channels pre
		// script-enforcement, so we'll add an offset from the
		// confirmation height of the channel.
		sid := lnwire.NewShortChanIDFromInt(channel.ChanId)
		expiryHeight := channel.ThawHeight + sid.BlockHeight
		chanLeaseExpiries[channel.ChannelPoint] = expiryHeight
	}

	return s.prepareLeasesResponse(
		ctx, accounts, batches, chanLeaseExpiries,
	)
}

// fetchNodeRatings returns an up to date set of ratings for each of the nodes
// we matched with in the passed batch.
func fetchNodeRatings(ctx context.Context, batches []*clientdb.LocalBatchSnapshot,
	auctioneer *auctioneer.Client) (map[[33]byte]poolrpc.NodeTier, error) {

	// As the node ratings call is batched, we'll first do a single query
	// to fetch all the ratings that we'll want to populate below.
	//
	// TODO(roasbeef): capture rating at time of lease? also cache
	nodeRatings := make(map[[33]byte]poolrpc.NodeTier)
	for _, batch := range batches {
		for _, matches := range batch.MatchedOrders {
			for _, match := range matches {
				nodeRatings[match.NodeKey] = 0
			}
		}
	}

	nodeKeys := make([]*btcec.PublicKey, 0, len(nodeRatings))
	for pubKey := range nodeRatings {
		nodeKey, err := btcec.ParsePubKey(
			pubKey[:], btcec.S256(),
		)
		if err != nil {
			return nil, err
		}

		nodeKeys = append(nodeKeys, nodeKey)
	}

	// Query for the latest node ranking information for each of the nodes
	// we gathered above.
	nodeRatingsResp, err := auctioneer.NodeRating(ctx, nodeKeys...)
	if err != nil {
		return nil, fmt.Errorf("unable to query "+
			"node tier: %v", err)
	}

	for _, nodeRating := range nodeRatingsResp.NodeRatings {
		var pubKey [33]byte
		copy(pubKey[:], nodeRating.NodePubkey)

		nodeRatings[pubKey] = nodeRating.NodeTier
	}

	return nodeRatings, nil
}

// prepareLeasesResponse prepares a poolrpc.LeasesResponse for the given
// accounts in the given batches.
func (s *rpcServer) prepareLeasesResponse(ctx context.Context,
	accounts map[[33]byte]struct{}, batches []*clientdb.LocalBatchSnapshot,
	chanLeaseExpiries map[string]uint32) (*poolrpc.LeasesResponse, error) {

	// First, we'll try to grab our set of node ratings as we want them to
	// fully populate the lease response.
	nodeRatings, err := fetchNodeRatings(ctx, batches, s.auctioneer)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch node ratings: %v", err)
	}

	// Now we're ready to prepare our response.
	var (
		rpcLeases      []*poolrpc.Lease
		totalAmtEarned btcutil.Amount
		totalAmtPaid   btcutil.Amount
	)
	for _, batch := range batches {
		// Keep a tally of how many channels were created within each
		// batch in order to estimate the chain fees paid.
		numChans := 0

		// Keep track of the index for the first lease found for this
		// batch. We'll use this to know where to start from when
		// populating the ChainFeeSat field for each lease.
		firstIdxForBatch := len(rpcLeases)

		for nonce, matches := range batch.MatchedOrders {
			nonce := nonce
			for _, match := range matches {
				// Obtain our order that was matched.
				ourOrder, ok := batch.Orders[nonce]
				if !ok {
					return nil, fmt.Errorf("order %v not "+
						"found in batch snapshot", nonce)
				}

				// Filter out the account if required.
				_, ok = accounts[ourOrder.Details().AcctKey]
				if len(accounts) != 0 && !ok {
					continue
				}

				// Derive the channel output script, which we'll
				// use to locate the channel outpoint within the
				// batch transaction.
				ourMultiSigKey, err := s.server.lndServices.WalletKit.DeriveKey(
					ctx, &ourOrder.Details().MultiSigKeyLocator,
				)
				if err != nil {
					return nil, err
				}
				chanAmt := match.UnitsFilled.ToSatoshis()
				_, chanOutput, err := input.GenFundingPkScript(
					ourMultiSigKey.PubKey.SerializeCompressed(),
					match.MultiSigKey[:], int64(chanAmt),
				)
				if err != nil {
					return nil, err
				}

				batchTxHash := batch.BatchTX.TxHash()
				chanOutputIdx, ok := poolscript.LocateOutputScript(
					batch.BatchTX, chanOutput.PkScript,
				)
				if !ok {
					return nil, fmt.Errorf("channel with "+
						"script %x not found in batch "+
						"transaction %v",
						chanOutput.PkScript, batchTxHash)
				}

				// The duration of the channel is always that
				// specified by the bid order.
				var bidDuration uint32
				if ourOrder.Type() == order.TypeBid {
					bidDuration = ourOrder.(*order.Bid).LeaseDuration
				} else {
					bidDuration = match.Order.(*order.Bid).LeaseDuration
				}

				// Calculate the premium paid/received to/from
				// the maker/taker and the execution fee paid
				// to the auctioneer and tally them.
				premium := batch.ClearingPrice.LumpSumPremium(
					chanAmt, bidDuration,
				)
				exeFee := batch.ExecutionFee.BaseFee() +
					batch.ExecutionFee.ExecutionFee(chanAmt)

				if ourOrder.Type() == order.TypeBid {
					totalAmtPaid += premium
				} else {
					totalAmtEarned += premium
				}
				totalAmtPaid += exeFee

				chanPointStr := fmt.Sprintf(
					"%v:%v", batchTxHash, chanOutputIdx,
				)
				leaseExpiryHeight := chanLeaseExpiries[chanPointStr]

				purchased := ourOrder.Type() == order.TypeBid
				nodeTier := nodeRatings[match.NodeKey]
				rpcLeases = append(rpcLeases, &poolrpc.Lease{
					ChannelPoint: &poolrpc.OutPoint{
						Txid:        batchTxHash[:],
						OutputIndex: chanOutputIdx,
					},
					ChannelAmtSat:         uint64(chanAmt),
					ChannelDurationBlocks: bidDuration,
					ChannelLeaseExpiry:    leaseExpiryHeight,
					PremiumSat:            uint64(premium),
					ClearingRatePrice:     uint64(batch.ClearingPrice),
					OrderFixedRate:        uint64(ourOrder.Details().FixedRate),
					ExecutionFeeSat:       uint64(exeFee),
					OrderNonce:            nonce[:],
					Purchased:             purchased,
					ChannelRemoteNodeKey:  match.NodeKey[:],
					ChannelNodeTier:       nodeTier,
				})

				numChans++
			}

			// Estimate the chain fees paid for the number of
			// channels created in this batch and tally them.
			chainFee := order.EstimateTraderFee(
				uint32(numChans), batch.BatchTxFeeRate,
			)

			// We'll need to compute the chain fee paid for each
			// lease. Since multiple leases can be created within a
			// batch, we'll need to evenly distribute. We'll always
			// apply the remainder to the first lease in the batch.
			chainFeePerChan := chainFee / btcutil.Amount(numChans)
			chainFeePerChanRem := chainFee % btcutil.Amount(numChans)
			for i := firstIdxForBatch; i < len(rpcLeases); i++ {
				chainFee := chainFeePerChan
				if i == firstIdxForBatch {
					chainFee += chainFeePerChanRem
				}
				rpcLeases[i].ChainFeeSat = uint64(chainFee)
			}
			totalAmtPaid += chainFee
		}
	}

	return &poolrpc.LeasesResponse{
		Leases:            rpcLeases,
		TotalAmtEarnedSat: uint64(totalAmtEarned),
		TotalAmtPaidSat:   uint64(totalAmtPaid),
	}, nil
}

// BatchSnapshot returns information about a target batch including the
// clearing price of the batch, and the set of orders matched within the batch.
func (s *rpcServer) BatchSnapshot(ctx context.Context,
	req *poolrpc.BatchSnapshotRequest) (*poolrpc.BatchSnapshotResponse, error) {

	// THe current version of this call just proxies the request to the
	// auctioneer, as we may only have information about the batch if we
	// were matched with it on disk.
	var batchID order.BatchID
	copy(batchID[:], req.BatchId)
	return s.auctioneer.BatchSnapshot(ctx, batchID)
}

// BatchSnapshots returns a list of batch snapshots starting at the start batch
// ID and going back through the history of batches, returning at most the
// number of specified batches. A maximum of 100 snapshots can be queried in
// one call. If no start batch ID is provided, the most recent finalized batch
// is used as the starting point to go back from.
func (s *rpcServer) BatchSnapshots(ctx context.Context,
	req *poolrpc.BatchSnapshotsRequest) (*poolrpc.BatchSnapshotsResponse,
	error) {

	return s.auctioneer.BatchSnapshots(ctx, req)
}

// LeaseDurations returns the current set of valid lease duration in the
// market as is, and also information w.r.t if the market is currently active.
func (s *rpcServer) LeaseDurations(ctx context.Context,
	_ *poolrpc.LeaseDurationRequest) (*poolrpc.LeaseDurationResponse, error) {

	auctionTerms, err := s.auctioneer.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to query auctioneer terms: %v",
			err)
	}

	return &poolrpc.LeaseDurationResponse{
		LeaseDurations: auctionTerms.LeaseDurations,
	}, nil
}

// NextBatchInfo returns information about the next batch the auctioneer
// will perform.
func (s *rpcServer) NextBatchInfo(ctx context.Context,
	_ *poolrpc.NextBatchInfoRequest) (*poolrpc.NextBatchInfoResponse, error) {

	auctionTerms, err := s.auctioneer.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to query auctioneer terms: %v",
			err)
	}

	return &poolrpc.NextBatchInfoResponse{
		ConfTarget:      auctionTerms.NextBatchConfTarget,
		FeeRateSatPerKw: uint64(auctionTerms.NextBatchFeeRate),
		ClearTimestamp:  uint64(auctionTerms.NextBatchClear.Unix()),
	}, nil
}

// NodeRatings returns rating information about the target node. This can be
// used to query the rating of your own node, or other nodes to determine which
// asks/bids might be filled based on a target min node tier.
func (s *rpcServer) NodeRatings(ctx context.Context,
	req *poolrpc.NodeRatingRequest) (*poolrpc.NodeRatingResponse, error) {

	if len(req.NodePubkeys) == 0 {
		return nil, fmt.Errorf("node pub key must be provided")
	}

	pubKeys := make([]*btcec.PublicKey, 0, len(req.NodePubkeys))
	for _, nodeKeyBytes := range req.NodePubkeys {
		nodeKey, err := btcec.ParsePubKey(nodeKeyBytes, btcec.S256())
		if err != nil {
			return nil, err
		}

		pubKeys = append(pubKeys, nodeKey)
	}

	return s.auctioneer.NodeRating(ctx, pubKeys...)
}

// rpcOrderStateToDBState maps the order state as received over the RPC
// protocol to the local state that we use in the database.
func rpcOrderStateToDBState(state poolrpc.OrderState) (order.State, error) {
	switch state {
	case poolrpc.OrderState_ORDER_SUBMITTED:
		return order.StateSubmitted, nil

	case poolrpc.OrderState_ORDER_CLEARED:
		return order.StateCleared, nil

	case poolrpc.OrderState_ORDER_PARTIALLY_FILLED:
		return order.StatePartiallyFilled, nil

	case poolrpc.OrderState_ORDER_EXECUTED:
		return order.StateExecuted, nil

	case poolrpc.OrderState_ORDER_CANCELED:
		return order.StateCanceled, nil

	case poolrpc.OrderState_ORDER_EXPIRED:
		return order.StateExpired, nil

	case poolrpc.OrderState_ORDER_FAILED:
		return order.StateFailed, nil

	default:
		return 0, fmt.Errorf("unknown state: %v", state)
	}
}

// DBOrderStateToRPCState maps the order state as stored in the database to the
// corresponding RPC enum type.
func DBOrderStateToRPCState(state order.State) (poolrpc.OrderState, error) {
	switch state {
	case order.StateSubmitted:
		return poolrpc.OrderState_ORDER_SUBMITTED, nil

	case order.StateCleared:
		return poolrpc.OrderState_ORDER_CLEARED, nil

	case order.StatePartiallyFilled:
		return poolrpc.OrderState_ORDER_PARTIALLY_FILLED, nil

	case order.StateExecuted:
		return poolrpc.OrderState_ORDER_EXECUTED, nil

	case order.StateCanceled:
		return poolrpc.OrderState_ORDER_CANCELED, nil

	case order.StateExpired:
		return poolrpc.OrderState_ORDER_EXPIRED, nil

	case order.StateFailed:
		return poolrpc.OrderState_ORDER_FAILED, nil

	default:
		return 0, fmt.Errorf("unknown state: %v", state)
	}
}

// dbMatchStateToRPCMatchState maps the match state as stored in the database to
// the corresponding RPC enum type.
func dbMatchStateToRPCMatchState(
	matchState order.MatchState) (poolrpc.MatchState, error) {

	switch matchState {
	case order.MatchStatePrepare:
		return poolrpc.MatchState_PREPARE, nil

	case order.MatchStateAccepted:
		return poolrpc.MatchState_ACCEPTED, nil

	case order.MatchStateRejected:
		return poolrpc.MatchState_REJECTED, nil

	case order.MatchStateSigned:
		return poolrpc.MatchState_SIGNED, nil

	case order.MatchStateFinalized:
		return poolrpc.MatchState_FINALIZED, nil

	default:
		return 0, fmt.Errorf("unknown match state: %v", matchState)
	}
}

// dbEventsToRPCEvents maps the events as stored in the database to the
// corresponding RPC types.
func dbEventsToRPCEvents(dbEvents []event.Event) ([]*poolrpc.OrderEvent,
	error) {

	rpcEvents := make([]*poolrpc.OrderEvent, 0, len(dbEvents))
	for _, record := range dbEvents {
		rpcEvent := &poolrpc.OrderEvent{
			TimestampNs: record.Timestamp().UnixNano(),
			EventStr:    record.String(),
		}

		// Decode by type to allow for further kinds of events.
		switch e := record.(type) {
		case *clientdb.CreatedEvent:
			// No additional information other than the timestamp
			// itself is stored for this event type.

		case *clientdb.UpdatedEvent:
			prevState, err := DBOrderStateToRPCState(e.PrevState)
			if err != nil {
				return nil, err
			}
			newState, err := DBOrderStateToRPCState(e.NewState)
			if err != nil {
				return nil, err
			}
			rpcEvent.Event = &poolrpc.OrderEvent_StateChange{
				StateChange: &poolrpc.UpdatedEvent{
					PreviousState: prevState,
					NewState:      newState,
					UnitsFilled:   uint32(e.UnitsFilled),
				},
			}

		case *clientdb.MatchEvent:
			matchState, err := dbMatchStateToRPCMatchState(
				e.MatchState,
			)
			if err != nil {
				return nil, err
			}
			rpcEvent.Event = &poolrpc.OrderEvent_Matched{
				Matched: &poolrpc.MatchEvent{
					MatchState:   matchState,
					UnitsFilled:  uint32(e.UnitsFilled),
					MatchedOrder: e.MatchedOrder[:],
					RejectReason: poolrpc.MatchRejectReason(
						e.RejectReason,
					),
				},
			}

		default:
			return nil, fmt.Errorf("unknown event type: %v", e)
		}

		rpcEvents = append(rpcEvents, rpcEvent)
	}

	return rpcEvents, nil
}

// nodeHasTorAddrs returns true if there exists a Tor address amongst the set
// of active Uris for a node.
func nodeHasTorAddrs(nodeAddrs []string) bool {
	for _, nodeAddr := range nodeAddrs {
		// Obtain the host to determine if this is a Tor address.
		host, _, err := net.SplitHostPort(nodeAddr)
		if err != nil {
			host = nodeAddr
		}

		if tor.IsOnionHost(host) {
			return true
		}
	}

	return false
}

// unmarshallNodeTier maps the RPC node tier enum to the node tier used in
// memory.
func unmarshallNodeTier(nodeTier poolrpc.NodeTier) (order.NodeTier, error) {
	switch nodeTier {
	case poolrpc.NodeTier_TIER_DEFAULT:
		return order.DefaultMinNodeTier, nil

	case poolrpc.NodeTier_TIER_0:
		return order.NodeTier0, nil

	case poolrpc.NodeTier_TIER_1:
		return order.NodeTier1, nil

	default:
		return 0, fmt.Errorf("unknown node tier: %v", nodeTier)
	}
}
