package llm

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/llm/account"
	"github.com/lightninglabs/llm/auctioneer"
	"github.com/lightninglabs/llm/chaninfo"
	"github.com/lightninglabs/llm/clientdb"
	"github.com/lightninglabs/llm/clmrpc"
	"github.com/lightninglabs/llm/order"
	"github.com/lightninglabs/llm/terms"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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
	fundingManager *fundingMgr

	// pendingOpenChannels is a channel through which we'll send received
	// notifications for pending open channels resulting from a successful
	// batch.
	pendingOpenChannels chan<- *lnrpc.ChannelEventUpdate_PendingOpenChannel

	quit                           chan struct{}
	wg                             sync.WaitGroup
	blockNtfnCancel                func()
	pendingOpenChannelStreamCancel func()
	recoveryMutex                  sync.Mutex
	recoveryPending                bool
}

// accountStore is a clientdb.DB wrapper to implement the account.Store
// interface.
type accountStore struct {
	*clientdb.DB
}

var _ account.Store = (*accountStore)(nil)

func (s *accountStore) PendingBatch() error {
	_, _, err := s.DB.PendingBatch()
	return err
}

// newRPCServer creates a new client-side RPC server that uses the given
// connection to the trader's lnd node and the auction server. A client side
// database is created in `serverDir` if it does not yet exist.
func newRPCServer(server *Server) *rpcServer {
	accountStore := &accountStore{server.db}
	lnd := &server.lndServices.LndServices
	pendingOpenChannels := make(chan *lnrpc.ChannelEventUpdate_PendingOpenChannel)
	quit := make(chan struct{})
	return &rpcServer{
		server:      server,
		lndServices: lnd,
		lndClient:   server.lndClient,
		auctioneer:  server.AuctioneerClient,
		accountManager: account.NewManager(&account.ManagerConfig{
			Store:          accountStore,
			Auctioneer:     server.AuctioneerClient,
			Wallet:         lnd.WalletKit,
			Signer:         lnd.Signer,
			ChainNotifier:  lnd.ChainNotifier,
			TxSource:       lnd.Client,
			TxFeeEstimator: lnd.Client,
		}),
		orderManager: order.NewManager(&order.ManagerConfig{
			Store:     server.db,
			AcctStore: accountStore,
			Lightning: lnd.Client,
			Wallet:    lnd.WalletKit,
			Signer:    lnd.Signer,
		}),
		fundingManager: &fundingMgr{
			db:                  server.db,
			walletKit:           lnd.WalletKit,
			lightningClient:     lnd.Client,
			baseClient:          server.lndClient,
			pendingOpenChannels: pendingOpenChannels,
			quit:                quit,
			batchStepTimeout:    defaultBatchStepTimeout,
			newNodesOnly:        server.cfg.NewNodesOnly,
		},
		pendingOpenChannels: pendingOpenChannels,
		quit:                quit,
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
	info, err := s.lndServices.Client.GetInfo(lndCtx)
	if err != nil {
		return fmt.Errorf("error in GetInfo: %v", err)
	}

	rpcLog.Infof("Connected to lnd node %v with pubkey %v", info.Alias,
		hex.EncodeToString(info.IdentityPubkey[:]))

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
		case s.pendingOpenChannels <- channel:
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
				err := s.server.Stop()
				if err != nil {
					rpcLog.Errorf("Error shutting down: %v",
						err)
				}
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
				err := s.server.Stop()
				if err != nil {
					rpcLog.Errorf("Error shutting down: %v",
						err)
				}
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
func (s *rpcServer) handleServerMessage(rpcMsg *clmrpc.ServerAuctionMessage) error {
	switch msg := rpcMsg.Msg.(type) {
	// A new batch has been assembled with some of our orders.
	case *clmrpc.ServerAuctionMessage_Prepare:
		// Parse and formally validate what we got from the server.
		rpcLog.Tracef("Received prepare msg from server, batch_id=%x: %v",
			msg.Prepare.BatchId, spew.Sdump(msg))
		batch, err := order.ParseRPCBatch(msg.Prepare)
		if err != nil {
			return fmt.Errorf("error parsing RPC batch: %v", err)
		}

		rpcLog.Infof("Received PrepareMsg for batch=%x, num_orders=%v",
			batch.ID[:], len(batch.MatchedOrders))

		// The prepare message can be sent over and over again if the
		// batch needs adjustment. Clear all previous shims.
		if s.orderManager.HasPendingBatch() {
			pendingBatch := s.orderManager.PendingBatch()
			err := pendingBatch.CancelPendingFundingShims(
				s.lndClient,
				func(o order.Nonce) (order.Order, error) {
					return s.server.db.GetOrder(o)
				},
			)
			if err != nil {
				// We can't accept the batch, something went
				// wrong.
				rpcLog.Errorf("Error clearing previous batch: "+
					"%v", err)
				return s.sendRejectBatch(batch, err)
			}

			// TODO(guggero): Also abandon any channels that might
			// still be pending from a previous round of the same
			// batch or a previous batch that we didn't make it into
			// the final round.
		}

		// Do an in-depth verification of the batch.
		err = s.orderManager.OrderMatchValidate(batch)
		if err != nil {
			// We can't accept the batch, something went wrong.
			rpcLog.Errorf("Error validating batch: %v", err)
			return s.sendRejectBatch(batch, err)
		}

		// Before we accept the batch, we'll finish preparations on our
		// end which include applying any order match predicates,
		// connecting out to peers, and registering funding shim.
		err = s.fundingManager.prepChannelFunding(batch)
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

	case *clmrpc.ServerAuctionMessage_Sign:
		// We were able to accept the batch. Inform the auctioneer,
		// then start negotiating with the remote peers. We'll sign
		// once all channel partners have responded.
		batch := s.orderManager.PendingBatch()
		channelKeys, err := s.fundingManager.batchChannelSetup(batch)
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
	case *clmrpc.ServerAuctionMessage_Finalize:
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
	req *clmrpc.QuoteAccountRequest) (*clmrpc.QuoteAccountResponse, error) {

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

	return &clmrpc.QuoteAccountResponse{
		MinerFeeRateSatPerKw: uint64(feeRate),
		MinerFeeTotal:        uint64(totalFee),
	}, nil
}

func (s *rpcServer) InitAccount(ctx context.Context,
	req *clmrpc.InitAccountRequest) (*clmrpc.Account, error) {

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

	account, err := s.accountManager.InitAccount(
		ctx, btcutil.Amount(req.AccountValue), expiryHeight,
		bestHeight, confTarget,
	)
	if err != nil {
		return nil, err
	}

	return marshallAccount(account)
}

func (s *rpcServer) ListAccounts(ctx context.Context,
	req *clmrpc.ListAccountsRequest) (*clmrpc.ListAccountsResponse, error) {

	accounts, err := s.server.db.Accounts()
	if err != nil {
		return nil, err
	}

	rpcAccounts := make([]*clmrpc.Account, 0, len(accounts))
	for _, account := range accounts {
		rpcAccount, err := marshallAccount(account)
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
	terms, err := s.auctioneer.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to query auctioneer terms: %v",
			err)
	}

	// For each active account, consume the worst-case account delta if the
	// order were to be matched.
	accountDebits := make(map[[33]byte]btcutil.Amount)
	auctionFeeSchedule := terms.FeeSchedule()
	for _, account := range accounts {
		var (
			debitAmt btcutil.Amount
			acctKey  [33]byte
		)

		copy(
			acctKey[:],
			account.TraderKey.PubKey.SerializeCompressed(),
		)

		// We'll make sure to assumulate a distinct sum for each
		// outstanding account the user has.
		for _, order := range orders {
			if order.Details().AcctKey != acctKey {
				continue
			}

			debitAmt += order.ReservedValue(auctionFeeSchedule)
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

	return &clmrpc.ListAccountsResponse{
		Accounts: rpcAccounts,
	}, nil
}

func marshallAccount(a *account.Account) (*clmrpc.Account, error) {
	var rpcState clmrpc.AccountState
	switch a.State {
	case account.StateInitiated, account.StatePendingOpen:
		rpcState = clmrpc.AccountState_PENDING_OPEN

	case account.StatePendingUpdate:
		rpcState = clmrpc.AccountState_PENDING_UPDATE

	case account.StateOpen:
		rpcState = clmrpc.AccountState_OPEN

	case account.StateExpired:
		rpcState = clmrpc.AccountState_EXPIRED

	case account.StatePendingClosed:
		rpcState = clmrpc.AccountState_PENDING_CLOSED

	case account.StateClosed:
		rpcState = clmrpc.AccountState_CLOSED

	case account.StateCanceledAfterRecovery:
		rpcState = clmrpc.AccountState_RECOVERY_FAILED

	case account.StatePendingBatch:
		rpcState = clmrpc.AccountState_PENDING_BATCH

	default:
		return nil, fmt.Errorf("unknown state %v", a.State)
	}

	// The latest transaction is only known after the account has been
	// funded.
	var latestTxHash chainhash.Hash
	if a.State != account.StateInitiated {
		latestTxHash = a.LatestTx.TxHash()
	}

	return &clmrpc.Account{
		TraderKey: a.TraderKey.PubKey.SerializeCompressed(),
		Outpoint: &clmrpc.OutPoint{
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
	req *clmrpc.DepositAccountRequest) (*clmrpc.DepositAccountResponse, error) {

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

	rpcModifiedAccount, err := marshallAccount(modifiedAccount)
	if err != nil {
		return nil, err
	}
	txHash := tx.TxHash()

	return &clmrpc.DepositAccountResponse{
		Account:     rpcModifiedAccount,
		DepositTxid: txHash[:],
	}, nil
}

// WithdrawAccount handles a trader's request to withdraw funds from the
// specified account by spending the current account output to the specified
// outputs.
func (s *rpcServer) WithdrawAccount(ctx context.Context,
	req *clmrpc.WithdrawAccountRequest) (*clmrpc.WithdrawAccountResponse, error) {

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

	rpcModifiedAccount, err := marshallAccount(modifiedAccount)
	if err != nil {
		return nil, err
	}
	txHash := tx.TxHash()

	return &clmrpc.WithdrawAccountResponse{
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
	req *clmrpc.BumpAccountFeeRequest) (*clmrpc.BumpAccountFeeResponse, error) {

	traderKey, err := btcec.ParsePubKey(req.TraderKey, btcec.S256())
	if err != nil {
		return nil, err
	}
	feeRate := chainfee.SatPerKWeight(req.FeeRateSatPerKw)

	err = s.accountManager.BumpAccountFee(ctx, traderKey, feeRate)
	if err != nil {
		return nil, err
	}

	return &clmrpc.BumpAccountFeeResponse{}, nil
}

// CloseAccount handles a trader's request to close the specified account.
func (s *rpcServer) CloseAccount(ctx context.Context,
	req *clmrpc.CloseAccountRequest) (*clmrpc.CloseAccountResponse, error) {

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
	case *clmrpc.CloseAccountRequest_OutputWithFee:
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
		case *clmrpc.OutputWithFee_ConfTarget:
			var err error
			feeRate, err = s.lndServices.WalletKit.EstimateFee(
				ctx, int32(feeExpr.ConfTarget),
			)
			if err != nil {
				return nil, err
			}

		case *clmrpc.OutputWithFee_FeeRateSatPerKw:
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
	case *clmrpc.CloseAccountRequest_Outputs:
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
		switch orderState {
		case order.StateFailed, order.StateExpired,
			order.StateCanceled, order.StateExecuted:
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

	return &clmrpc.CloseAccountResponse{
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

// parseRPCOutputs maps []*clmrpc.Output -> []*wire.TxOut.
func (s *rpcServer) parseRPCOutputs(outputs []*clmrpc.Output) ([]*wire.TxOut,
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
	_ *clmrpc.RecoverAccountsRequest) (*clmrpc.RecoverAccountsResponse,
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

	return &clmrpc.RecoverAccountsResponse{
		NumRecoveredAccounts: uint32(numRecovered),
	}, nil
}

// SubmitOrder assembles all the information that is required to submit an order
// from the trader's lnd node, signs it and then sends the order to the server
// to be included in the auctioneer's order book.
func (s *rpcServer) SubmitOrder(ctx context.Context,
	req *clmrpc.SubmitOrderRequest) (*clmrpc.SubmitOrderResponse, error) {

	var o order.Order
	switch requestOrder := req.Details.(type) {
	case *clmrpc.SubmitOrderRequest_Ask:
		a := requestOrder.Ask
		kit, err := order.ParseRPCOrder(a.Version, a.Details)
		if err != nil {
			return nil, err
		}
		o = &order.Ask{
			Kit:         *kit,
			MaxDuration: a.MaxDurationBlocks,
		}

	case *clmrpc.SubmitOrderRequest_Bid:
		b := requestOrder.Bid
		kit, err := order.ParseRPCOrder(b.Version, b.Details)
		if err != nil {
			return nil, err
		}
		o = &order.Bid{
			Kit:         *kit,
			MinDuration: b.MinDurationBlocks,
		}

	default:
		return nil, fmt.Errorf("invalid order request")
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
	terms, err := s.auctioneer.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not query auctioneer terms: %v",
			err)
	}

	// Collect all the order data and sign it before sending it to the
	// auction server.
	serverParams, err := s.orderManager.PrepareOrder(ctx, o, acct, terms)
	if err != nil {
		return nil, err
	}

	// Send the order to the server. If this fails, then the order is
	// certain to never get into the order book. We don't need to keep it
	// around in that case.
	err = s.auctioneer.SubmitOrder(ctx, o, serverParams)
	if err != nil {
		// TODO(guggero): Put in state failed instead of removing?
		if err2 := s.server.db.DelOrder(o.Nonce()); err2 != nil {
			rpcLog.Errorf("Could not delete failed order: %v", err2)
		}

		// If there was something wrong with the information the user
		// provided, then return this as a nice string instead of an
		// error type.
		if userErr, ok := err.(*order.UserError); ok {
			rpcLog.Warnf("Invalid order details: %v", userErr)

			return &clmrpc.SubmitOrderResponse{
				Details: &clmrpc.SubmitOrderResponse_InvalidOrder{
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
	return &clmrpc.SubmitOrderResponse{
		Details: &clmrpc.SubmitOrderResponse_AcceptedOrderNonce{
			AcceptedOrderNonce: orderNonce[:],
		},
	}, nil
}

// ListOrders returns a list of all orders that is currently known to the trader
// client's local store. The state of each order is queried on the auction
// server and returned as well.
func (s *rpcServer) ListOrders(ctx context.Context, _ *clmrpc.ListOrdersRequest) (
	*clmrpc.ListOrdersResponse, error) {

	// Get all orders from our local store first.
	dbOrders, err := s.server.db.GetOrders()
	if err != nil {
		return nil, err
	}

	// If we cannot query the auctioneer, we just use an empty fee
	// schedule, as it is only used for calculating the reserved value.
	var feeSchedule terms.FeeSchedule = terms.NewLinearFeeSchedule(0, 0)
	terms, err := s.auctioneer.Terms(ctx)
	if err != nil {
		log.Warnf("unable to query auctioneer terms: %v", err)
	} else {
		feeSchedule = terms.FeeSchedule()
	}

	// The RPC is split by order type so we have to separate them now.
	asks := make([]*clmrpc.Ask, 0, len(dbOrders))
	bids := make([]*clmrpc.Bid, 0, len(dbOrders))
	for _, dbOrder := range dbOrders {
		nonce := dbOrder.Nonce()
		dbDetails := dbOrder.Details()

		var orderState clmrpc.OrderState
		switch dbDetails.State {

		case order.StateSubmitted:
			orderState = clmrpc.OrderState_ORDER_SUBMITTED

		case order.StateCleared:
			orderState = clmrpc.OrderState_ORDER_CLEARED

		case order.StatePartiallyFilled:
			orderState = clmrpc.OrderState_ORDER_PARTIALLY_FILLED

		case order.StateExecuted:
			orderState = clmrpc.OrderState_ORDER_EXECUTED

		case order.StateCanceled:
			orderState = clmrpc.OrderState_ORDER_CANCELED

		case order.StateExpired:
			orderState = clmrpc.OrderState_ORDER_EXPIRED

		case order.StateFailed:
			orderState = clmrpc.OrderState_ORDER_FAILED

		default:
			return nil, fmt.Errorf("unknown state: %v",
				dbDetails.State)
		}

		details := &clmrpc.Order{
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
		}

		switch o := dbOrder.(type) {
		case *order.Ask:
			rpcAsk := &clmrpc.Ask{
				Details:           details,
				MaxDurationBlocks: o.MaxDuration,
				Version:           uint32(o.Version),
			}
			asks = append(asks, rpcAsk)

		case *order.Bid:
			rpcBid := &clmrpc.Bid{
				Details:           details,
				MinDurationBlocks: o.MinDuration,
				Version:           uint32(o.Version),
			}
			bids = append(bids, rpcBid)

		default:
			return nil, fmt.Errorf("unknown order type: %v",
				o)
		}
	}
	return &clmrpc.ListOrdersResponse{
		Asks: asks,
		Bids: bids,
	}, nil
}

// CancelOrder cancels the order on the server and updates the state of the
// local order accordingly.
func (s *rpcServer) CancelOrder(ctx context.Context,
	req *clmrpc.CancelOrderRequest) (*clmrpc.CancelOrderResponse, error) {

	var nonce order.Nonce
	copy(nonce[:], req.OrderNonce)

	rpcLog.Infof("Cancelling order_nonce=%v", nonce)

	// We cancel an order in two phases. First, we'll cancel the order on
	// the server-side.
	err := s.auctioneer.CancelOrder(ctx, nonce)
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

	return &clmrpc.CancelOrderResponse{}, nil
}

// sendRejectBatch sends a reject message to the server with the properly
// decoded reason code and the full reason message as a string.
func (s *rpcServer) sendRejectBatch(batch *order.Batch, failure error) error {
	// As we're rejecting this batch, we'll now cancel all funding shims
	// that we may have registered since we may be matched with a distinct
	// set of channels if this batch is repeated.
	err := batch.CancelPendingFundingShims(
		s.lndClient,
		func(o order.Nonce) (order.Order, error) {
			return s.server.db.GetOrder(o)
		},
	)
	if err != nil {
		return err
	}

	msg := &clmrpc.ClientAuctionMessage_Reject{
		Reject: &clmrpc.OrderMatchReject{
			BatchId: batch.ID[:],
			Reason:  failure.Error(),
		},
	}

	// Attach the status code to the message to give a bit more context.
	var partialReject *matchRejectErr
	switch {
	case errors.Is(failure, order.ErrVersionMismatch):
		msg.Reject.ReasonCode = clmrpc.OrderMatchReject_BATCH_VERSION_MISMATCH

	case errors.Is(failure, order.ErrMismatchErr):
		msg.Reject.ReasonCode = clmrpc.OrderMatchReject_SERVER_MISBEHAVIOR

	case errors.As(failure, &partialReject):
		msg.Reject.ReasonCode = clmrpc.OrderMatchReject_PARTIAL_REJECT
		msg.Reject.RejectedOrders = make(map[string]*clmrpc.OrderReject)
		for nonce, reject := range partialReject.rejectedOrders {
			msg.Reject.RejectedOrders[nonce.String()] = reject
		}

	default:
		msg.Reject.ReasonCode = clmrpc.OrderMatchReject_UNKNOWN
	}

	rpcLog.Infof("Sending batch rejection message for batch %x with "+
		"code %v and message: %v", batch.ID, msg.Reject.ReasonCode,
		failure)

	// Send the message to the server. If a new error happens we return that
	// one because we know the causing error has at least been logged at
	// some point before.
	err = s.auctioneer.SendAuctionMessage(&clmrpc.ClientAuctionMessage{
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

	// Send the message to the server.
	return s.auctioneer.SendAuctionMessage(&clmrpc.ClientAuctionMessage{
		Msg: &clmrpc.ClientAuctionMessage_Accept{
			Accept: &clmrpc.OrderMatchAccept{
				BatchId: batch.ID[:],
			},
		},
	})
}

// GetLsatTokens returns all tokens that are contained in the LSAT token store.
func (s *rpcServer) GetLsatTokens(_ context.Context,
	_ *clmrpc.TokensRequest) (*clmrpc.TokensResponse, error) {

	log.Infof("Get LSAT tokens request received")

	tokens, err := s.server.lsatStore.AllTokens()
	if err != nil {
		return nil, err
	}

	rpcTokens := make([]*clmrpc.LsatToken, len(tokens))
	idx := 0
	for key, token := range tokens {
		macBytes, err := token.BaseMacaroon().MarshalBinary()
		if err != nil {
			return nil, err
		}
		rpcTokens[idx] = &clmrpc.LsatToken{
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

	return &clmrpc.TokensResponse{Tokens: rpcTokens}, nil
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

	rpcChannelInfos := make(map[string]*clmrpc.ChannelInfo, len(chanInfos))
	for chanPoint, chanInfo := range chanInfos {
		var channelType clmrpc.ChannelType
		switch chanInfo.Version {
		case chanbackup.TweaklessCommitVersion:
			channelType = clmrpc.ChannelType_TWEAKLESS
		case chanbackup.AnchorsCommitVersion:
			channelType = clmrpc.ChannelType_ANCHORS
		default:
			return fmt.Errorf("unknown channel type: %v",
				chanInfo.Version)
		}
		rpcChannelInfos[chanPoint.String()] = &clmrpc.ChannelInfo{
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

	return s.auctioneer.SendAuctionMessage(&clmrpc.ClientAuctionMessage{
		Msg: &clmrpc.ClientAuctionMessage_Sign{
			Sign: &clmrpc.OrderMatchSign{
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
	_ *clmrpc.AuctionFeeRequest) (*clmrpc.AuctionFeeResponse, error) {

	terms, err := s.auctioneer.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to query auctioneer terms: %v",
			err)
	}

	// TODO(roasbeef): accept the amt of order instead?
	return &clmrpc.AuctionFeeResponse{
		ExecutionFee: &clmrpc.ExecutionFee{
			BaseFee: uint64(terms.OrderExecBaseFee),
			FeeRate: uint64(terms.OrderExecFeeRate),
		},
	}, nil
}

// BatchSnapshot returns information about a target batch including the
// clearing price of the batch, and the set of orders matched within the batch.
func (s *rpcServer) BatchSnapshot(ctx context.Context,
	req *clmrpc.BatchSnapshotRequest) (*clmrpc.BatchSnapshotResponse, error) {

	// THe current version of this call just proxies the request to the
	// auctioneer, as we may only have information about the batch if we
	// were matched with it on disk.
	var batchID order.BatchID
	copy(batchID[:], req.BatchId)
	return s.auctioneer.BatchSnapshot(ctx, batchID)
}

// rpcOrderStateToDBState maps the order state as received over the RPC
// protocol to the local state that we use in the database.
func rpcOrderStateToDBState(state clmrpc.OrderState) (order.State, error) {

	var orderState order.State
	switch state {

	case clmrpc.OrderState_ORDER_SUBMITTED:
		orderState = order.StateSubmitted

	case clmrpc.OrderState_ORDER_CLEARED:
		orderState = order.StateCleared

	case clmrpc.OrderState_ORDER_PARTIALLY_FILLED:
		orderState = order.StatePartiallyFilled

	case clmrpc.OrderState_ORDER_EXECUTED:
		orderState = order.StateExecuted

	case clmrpc.OrderState_ORDER_CANCELED:
		orderState = order.StateCanceled

	case clmrpc.OrderState_ORDER_EXPIRED:
		orderState = order.StateExpired

	case clmrpc.OrderState_ORDER_FAILED:
		orderState = order.StateFailed

	default:
		return 0, fmt.Errorf("unknown state: %v", state)
	}

	return orderState, nil
}
