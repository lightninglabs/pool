package pool

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/chaninfo"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/event"
	"github.com/lightninglabs/pool/funding"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/chanbackup"
	lndFunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
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

	// Required by the grpc-gateway/v2 library for forward compatibility.
	// Must be after the atomically used variables to not break struct
	// alignment.
	poolrpc.UnimplementedTraderServer

	server         *Server
	lndServices    *lndclient.LndServices
	lndClient      lnrpc.LightningClient
	auctioneer     *auctioneer.Client
	accountManager account.Manager
	orderManager   order.Manager
	marshaler      Marshaler

	quit            chan struct{}
	wg              sync.WaitGroup
	blockNtfnCancel func()
	recoveryMutex   sync.Mutex
	recoveryPending bool

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
func newRPCServer(server *Server) (*rpcServer, error) {
	accountStore := &accountStore{server.db}
	lndServices := &server.lndServices.LndServices
	batchVersion, err := server.determineBatchVersion()
	if err != nil {
		return nil, err
	}
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
			ChainParams:    lndServices.ChainParams,
			LndVersion:     lndServices.Version,
		}),
		orderManager: order.NewManager(&order.ManagerConfig{
			Store:        server.db,
			AcctStore:    accountStore,
			Lightning:    lndServices.Client,
			Wallet:       lndServices.WalletKit,
			Signer:       lndServices.Signer,
			BatchVersion: batchVersion,
		}),
		marshaler: NewMarshaler(&marshalerConfig{
			GetOrders: server.db.GetOrders,
			Terms:     server.AuctioneerClient.Terms,
		}),
		quit: make(chan struct{}),
	}, nil
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
	if err := s.server.fundingManager.Start(); err != nil {
		return fmt.Errorf("unable to start funding manager: %v", err)
	}
	if err := s.server.sidecarAcceptor.Start(blockErrChan); err != nil {
		return fmt.Errorf("unable to start sidecar acceptor: %v", err)
	}

	s.wg.Add(1)
	go s.serverHandler(blockChan, blockErrChan)

	rpcLog.Infof("Trader server is now active")

	return nil
}

// Stop stops the server.
func (s *rpcServer) Stop() error {
	if !atomic.CompareAndSwapUint32(&s.stopped, 0, 1) {
		return nil
	}

	var returnErr error
	rpcLog.Info("Trader server stopping")
	if err := s.server.sidecarAcceptor.Stop(); err != nil {
		rpcLog.Errorf("Error stopping sidecar acceptor: %v", err)
		returnErr = err
	}
	if err := s.server.fundingManager.Stop(); err != nil {
		rpcLog.Errorf("Error stopping funding manager: %v", err)
	}
	s.accountManager.Stop()
	s.orderManager.Stop()
	if err := s.auctioneer.Stop(); err != nil {
		rpcLog.Errorf("Error closing server stream: %v", err)
	}

	close(s.quit)

	s.wg.Wait()
	s.blockNtfnCancel()

	rpcLog.Info("Stopped trader server")

	return returnErr
}

// serverHandler is the main event loop of the server.
func (s *rpcServer) serverHandler(blockChan chan int32,
	blockErrChan chan error) {

	defer s.wg.Done()

	for {
		select {
		case msg := <-s.auctioneer.FromServerChan:
			// An empty message means the client is shutting down.
			if msg == nil {
				continue
			}

			rpcLog.Debugf("Received message from the server: %s",
				poolrpc.PrintMsg(msg))
			err := s.handleServerMessage(msg)

			// Only shut down if this was a terminal error, and not
			// a batch reject (should rarely happen, but it's
			// possible).
			if err != nil && !errors.Is(err, order.ErrMismatchErr) {
				rpcLog.Errorf("Error handling server message: "+
					"%v", err)
				s.server.cfg.RequestShutdown()
			}

		case err := <-s.auctioneer.StreamErrChan:
			// If the server is shutting down, then the client has
			// already scheduled a restart. We only need to handle
			// other errors here.
			if err != nil && err != auctioneer.ErrServerShutdown {
				rpcLog.Errorf("Error in server stream: %v", err)
				err := s.auctioneer.HandleServerShutdown(err)
				if err != nil {
					rpcLog.Errorf("Error closing stream: "+
						"%v", err)
				}
			}

			rpcLog.Errorf("Unknown server error: %v", err)

		case height := <-blockChan:
			rpcLog.Infof("Received new block notification: "+
				"height=%v", height)
			s.updateHeight(height)

		case err := <-blockErrChan:
			if err != nil {
				rpcLog.Errorf("Unable to receive block "+
					"notification: %v", err)
				s.server.cfg.RequestShutdown()
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
func (s *rpcServer) handleServerMessage(
	rpcMsg *auctioneerrpc.ServerAuctionMessage) error {

	switch msg := rpcMsg.Msg.(type) {
	// A new batch has been assembled with some of our orders.
	case *auctioneerrpc.ServerAuctionMessage_Prepare:
		// Parse and formally validate what we got from the server.
		rpcLog.Tracef("Received prepare msg from server, "+
			"batch_id=%x: %v", msg.Prepare.BatchId,
			poolrpc.PrintMsg(msg.Prepare))
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

			// Clear our staging area for the new batch proposal. We
			// consider any errors as a hard failure and reject the
			// batch.
			err = s.server.fundingManager.DeletePendingBatch()
			if err != nil {
				rpcLog.Errorf("Error clearing previous "+
					"pending batch: %v", err)
				return s.sendRejectBatch(batch, err)
			}
		}

		// Do an in-depth verification of the batch.
		bestHeight := atomic.LoadUint32(&s.bestHeight)
		err = s.orderManager.OrderMatchValidate(batch, bestHeight)
		if err != nil {
			// We can't accept the batch, something went wrong.
			rpcLog.Errorf("Error validating batch: %v", err)
			return s.sendRejectBatch(batch, err)
		}

		// Before we accept the batch, we'll finish preparations on our
		// end which include applying any order match predicates,
		// connecting out to peers, and registering funding shim.
		err = s.server.fundingManager.PrepChannelFunding(
			batch, s.server.db.GetOrder,
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

	case *auctioneerrpc.ServerAuctionMessage_Sign:
		batch := s.orderManager.PendingBatch()

		// There is some auxiliary information in the "sign" message
		// that we need for the MuSig2/Taproot signing, let's try to
		// parse that first.
		serverNonces, prevOutputs, err := order.ParseRPCSign(msg.Sign)
		if err != nil {
			rpcLog.Errorf("Error parsing sign aux info: %v", err)
			return s.sendRejectBatch(batch, err)
		}
		batch.ServerNonces = serverNonces
		batch.PreviousOutputs = prevOutputs

		// We were able to accept the batch. Inform the auctioneer,
		// then start negotiating with the remote peers. We'll sign
		// once all channel partners have responded.
		channelKeys, err := s.server.fundingManager.BatchChannelSetup(
			batch,
		)
		if err != nil {
			rpcLog.Errorf("Error setting up channels: %v", err)
			return s.sendRejectBatch(batch, err)
		}

		rpcLog.Infof("Received OrderMatchSignBegin for batch=%x, "+
			"num_orders=%v", batch.ID[:], len(batch.MatchedOrders))

		// Sign for the accounts in the batch.
		sigs, nonces, err := s.orderManager.BatchSign()
		if err != nil {
			rpcLog.Errorf("Error signing batch: %v", err)
			return s.sendRejectBatch(batch, err)
		}
		err = s.sendSignBatch(batch, sigs, nonces, channelKeys)
		if err != nil {
			rpcLog.Errorf("Error sending sign msg: %v", err)
			return s.sendRejectBatch(batch, err)
		}

	// The previously prepared batch has been executed and we can finalize
	// it by opening the channel and persisting the account and order diffs.
	case *auctioneerrpc.ServerAuctionMessage_Finalize:
		rpcLog.Tracef("Received finalize msg from server, "+
			"batch_id=%x: %v", msg.Finalize.BatchId,
			poolrpc.PrintMsg(msg.Finalize))

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

		// If we were the provider for any sidecar channels, we want to
		// update our own state of the tickets to complete now.
		for ourOrderNonce := range batch.MatchedOrders {
			if err := s.setTicketStateForOrder(
				sidecar.StateCompleted, ourOrderNonce,
			); err != nil {
				rpcLog.Errorf("Unable to update our sidecar "+
					"ticket after completing batch: %v",
					err)
			}
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
		return fmt.Errorf("unknown server message: %v",
			poolrpc.PrintMsg(rpcMsg))
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
	case req.GetAbsoluteHeight() != 0 && req.GetRelativeHeight() != 0:
		return nil, fmt.Errorf("you must set only one of the relative " +
			"and absolute height parameters")

	case req.GetAbsoluteHeight() != 0:
		expiryHeight = req.GetAbsoluteHeight()

	case req.GetRelativeHeight() != 0:
		expiryHeight = req.GetRelativeHeight() + bestHeight

	default:
		return nil, fmt.Errorf("either relative or absolute height " +
			"must be specified")
	}

	var feeRate chainfee.SatPerKWeight

	switch {
	case req.GetFeeRateSatPerKw() > 0 && req.GetConfTarget() > 0:
		return nil, fmt.Errorf("you must set only one of the sats/kw " +
			"and confirmation target parameters")

	case req.GetFeeRateSatPerKw() > 0:
		feeRate = chainfee.SatPerKWeight(req.GetFeeRateSatPerKw())

	case req.GetConfTarget() > 0:
		// Determine the desired transaction fee.
		value := btcutil.Amount(req.AccountValue)
		confTarget := req.GetConfTarget()

		var err error
		feeRate, _, err = s.accountManager.QuoteAccount(
			ctx, value, confTarget,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to estimate on-chain fees: "+
				"%v", err)
		}

		// If the fee estimation ever returns a value too small
		// we set it to a valid minimum
		if feeRate < chainfee.FeePerKwFloor {
			feeRate = chainfee.FeePerKwFloor
		}

		log.Infof("Estimated total chain fee of %v for new account with "+
			"value=%v, conf_target=%v", feeRate, value, confTarget)

	default:
		return nil, fmt.Errorf("either sats/kw or confirmation target " +
			"must be specified")
	}

	// In order to be able to choose a default value (or to validate the
	// account version), we need to know if the version of the connected lnd
	// supports Taproot.
	version, err := s.determineAccountVersion(req.Version)
	if err != nil {
		return nil, err
	}

	if feeRate < chainfee.FeePerKwFloor {
		return nil, fmt.Errorf("fee rate of %d sat/kw is too low, "+
			"minimum is %d sat/kw", feeRate, chainfee.FeePerKwFloor)
	}

	acct, err := s.accountManager.InitAccount(
		ContextWithInitiator(ctx, req.Initiator),
		btcutil.Amount(req.AccountValue), version, feeRate,
		expiryHeight, bestHeight,
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

	validAccounts := make([]*account.Account, 0, len(accounts))
	for _, acct := range accounts {
		// Filter out inactive accounts if requested by the user.
		if req.ActiveOnly && !acct.State.IsActive() {
			continue
		}

		validAccounts = append(validAccounts, acct)
	}

	rpcAccounts, err := s.marshaler.MarshallAccountsWithAvailableBalance(
		ctx, validAccounts,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to list marshalled accounts: "+
			"%v", err)
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

	case account.StateExpiredPendingUpdate:
		rpcState = poolrpc.AccountState_PENDING_UPDATE

	default:
		return nil, fmt.Errorf("unknown state %v", a.State)
	}

	var rpcVersion poolrpc.AccountVersion
	switch a.Version {
	case account.VersionInitialNoVersion:
		rpcVersion = poolrpc.AccountVersion_ACCOUNT_VERSION_LEGACY

	case account.VersionTaprootEnabled:
		rpcVersion = poolrpc.AccountVersion_ACCOUNT_VERSION_TAPROOT

	case account.VersionMuSig2V100RC2:
		rpcVersion = poolrpc.AccountVersion_ACCOUNT_VERSION_TAPROOT_V2

	default:
		return nil, fmt.Errorf("unknown version <%d>", a.Version)
	}

	// The latest transaction is only known after the account has been
	// funded.
	var latestTxHash chainhash.Hash
	if a.LatestTx != nil {
		latestTxHash = a.LatestTx.TxHash()
	}

	return &poolrpc.Account{
		TraderKey: a.TraderKey.PubKey.SerializeCompressed(),
		Outpoint: &auctioneerrpc.OutPoint{
			Txid:        a.OutPoint.Hash[:],
			OutputIndex: a.OutPoint.Index,
		},
		Value:            uint64(a.Value),
		ExpirationHeight: a.Expiry,
		State:            rpcState,
		LatestTxid:       latestTxHash[:],
		Version:          rpcVersion,
	}, nil
}

// DepositAccount handles a trader's request to deposit funds into the specified
// account by spending the specified inputs.
func (s *rpcServer) DepositAccount(ctx context.Context,
	req *poolrpc.DepositAccountRequest) (*poolrpc.DepositAccountResponse,
	error) {

	rpcLog.Infof("Depositing %v into acct=%x",
		btcutil.Amount(req.AmountSat), req.TraderKey)

	// Ensure the trader key is well formed.
	traderKey, err := btcec.ParsePubKey(req.TraderKey)
	if err != nil {
		return nil, err
	}

	// Enforce a minimum fee rate of 253 sat/kw.
	feeRate := chainfee.SatPerKWeight(req.FeeRateSatPerKw)
	if feeRate < chainfee.FeePerKwFloor {
		return nil, fmt.Errorf("fee rate of %d sat/kw is too low, "+
			"minimum is %d sat/kw", feeRate, chainfee.FeePerKwFloor)
	}

	bestHeight := atomic.LoadUint32(&s.bestHeight)

	// If provided, determine new expiration value.
	var expiryHeight uint32
	switch {
	case req.GetAbsoluteExpiry() != 0 && req.GetRelativeExpiry() != 0:
		return nil, errors.New("relative and absolute height cannot " +
			"be set in the same request")
	case req.GetAbsoluteExpiry() != 0:
		expiryHeight = req.GetAbsoluteExpiry()
	case req.GetRelativeExpiry() != 0:
		expiryHeight = req.GetRelativeExpiry() + bestHeight
	}

	// In order to be able to choose a default value (or to validate the
	// account version), we need to know if the version of the connected lnd
	// supports Taproot.
	version, err := s.determineAccountVersion(req.NewVersion)
	if err != nil {
		return nil, err
	}

	// Proceed to process the deposit and map its response to the RPC's
	// response.
	modifiedAccount, tx, err := s.accountManager.DepositAccount(
		ctx, traderKey, btcutil.Amount(req.AmountSat), feeRate,
		bestHeight, expiryHeight, version,
	)
	if err != nil {
		return nil, err
	}

	rpcModAccounts, err := s.marshaler.MarshallAccountsWithAvailableBalance(
		ctx, []*account.Account{modifiedAccount},
	)
	if err != nil {
		return nil, err
	}
	txHash := tx.TxHash()

	return &poolrpc.DepositAccountResponse{
		Account:     rpcModAccounts[0],
		DepositTxid: txHash[:],
	}, nil
}

// WithdrawAccount handles a trader's request to withdraw funds from the
// specified account by spending the current account output to the specified
// outputs.
func (s *rpcServer) WithdrawAccount(ctx context.Context,
	req *poolrpc.WithdrawAccountRequest) (*poolrpc.WithdrawAccountResponse,
	error) {

	rpcLog.Infof("Withdrawing from acct=%x", req.TraderKey)

	// Ensure the trader key is well formed.
	traderKey, err := btcec.ParsePubKey(req.TraderKey)
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

	bestHeight := atomic.LoadUint32(&s.bestHeight)

	// If provided, determine new expiration value.
	var expiryHeight uint32
	switch {
	case req.GetAbsoluteExpiry() != 0 && req.GetRelativeExpiry() != 0:
		return nil, errors.New("relative and absolute height cannot " +
			"be set in the same request")
	case req.GetAbsoluteExpiry() != 0:
		expiryHeight = req.GetAbsoluteExpiry()
	case req.GetRelativeExpiry() != 0:
		expiryHeight = req.GetRelativeExpiry() + bestHeight
	}

	// In order to be able to choose a default value (or to validate the
	// account version), we need to know if the version of the connected lnd
	// supports Taproot.
	version, err := s.determineAccountVersion(req.NewVersion)
	if err != nil {
		return nil, err
	}

	// Proceed to process the withdrawal and map its response to the RPC's
	// response.
	modifiedAccount, tx, err := s.accountManager.WithdrawAccount(
		ctx, traderKey, outputs, feeRate, bestHeight, expiryHeight,
		version,
	)
	if err != nil {
		return nil, err
	}

	rpcModAccounts, err := s.marshaler.MarshallAccountsWithAvailableBalance(
		ctx, []*account.Account{modifiedAccount},
	)
	if err != nil {
		return nil, err
	}
	txHash := tx.TxHash()

	return &poolrpc.WithdrawAccountResponse{
		Account:      rpcModAccounts[0],
		WithdrawTxid: txHash[:],
	}, nil
}

// RenewAccount updates the expiration of an open/expired account. This
// will always require a signature from the auctioneer, even after the account
// has expired, to ensure the auctioneer is aware the account is being renewed.
func (s *rpcServer) RenewAccount(ctx context.Context,
	req *poolrpc.RenewAccountRequest) (*poolrpc.RenewAccountResponse,
	error) {

	rpcLog.Infof("Updating account expiration for account %x", req.AccountKey)

	// Ensure the account key is well formed.
	accountKey, err := btcec.ParsePubKey(req.AccountKey)
	if err != nil {
		return nil, err
	}

	// Determine the desired expiration value, can be relative or absolute.
	bestHeight := atomic.LoadUint32(&s.bestHeight)
	var expiryHeight uint32
	switch {
	case req.GetAbsoluteExpiry() != 0:
		expiryHeight = req.GetAbsoluteExpiry()
	case req.GetRelativeExpiry() != 0:
		expiryHeight = req.GetRelativeExpiry() + bestHeight
	default:
		return nil, errors.New("either relative or absolute height " +
			"must be specified")
	}

	// Enforce a minimum fee rate of 253 sat/kw.
	feeRate := chainfee.SatPerKWeight(req.FeeRateSatPerKw)
	if feeRate < chainfee.FeePerKwFloor {
		return nil, fmt.Errorf("fee rate of %d sat/kw is too low, "+
			"minimum is %d sat/kw", feeRate, chainfee.FeePerKwFloor)
	}

	// In order to be able to choose a default value (or to validate the
	// account version), we need to know if the version of the connected lnd
	// supports Taproot.
	version, err := s.determineAccountVersion(req.NewVersion)
	if err != nil {
		return nil, err
	}

	// Proceed to process the expiration update and map its response to the
	// RPC's response.
	modifiedAccount, tx, err := s.accountManager.RenewAccount(
		ctx, accountKey, expiryHeight, feeRate, bestHeight, version,
	)
	if err != nil {
		return nil, err
	}

	rpcModAccounts, err := s.marshaler.MarshallAccountsWithAvailableBalance(
		ctx, []*account.Account{modifiedAccount},
	)
	if err != nil {
		return nil, err
	}
	txHash := tx.TxHash()

	return &poolrpc.RenewAccountResponse{
		Account:     rpcModAccounts[0],
		RenewalTxid: txHash[:],
	}, nil
}

// BumpAccountFee attempts to bump the fee of an account's transaction through
// child-pays-for-parent (CPFP). Since the CPFP is performed through the backing
// lnd node, the account transaction must contain an output under its control
// for a successful bump. If a CPFP has already been performed for an account,
// and this RPC is invoked again, then a replacing transaction (RBF) of the
// child will be broadcast.
func (s *rpcServer) BumpAccountFee(ctx context.Context,
	req *poolrpc.BumpAccountFeeRequest) (*poolrpc.BumpAccountFeeResponse,
	error) {

	traderKey, err := btcec.ParsePubKey(req.TraderKey)
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

	traderKey, err := btcec.ParsePubKey(req.TraderKey)
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
			feeRate, err = s.lndServices.WalletKit.EstimateFeeRate(
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

// serverAssistedRecovery executes the server assisted account recovery process.
func (s *rpcServer) serverAssistedRecovery(ctx context.Context, target uint32) (
	[]*account.Account, error) {

	// The account recovery process uses a bi-directional streaming RPC on
	// the server side. Unfortunately, because of the way streaming RPCs
	// work, the LSAT interceptor isn't able to _purchase_ a token during
	// a streaming RPC call (the 402/payment required error is only returned
	// after the interceptor was handed the call, so it cannot act on it
	// anymore). But since a user that has lost their data most likely als
	// lost their LSAT, the recovery will fail if this is the first call to
	// the server ever. That's why we call an RPC that's definitely not on
	// the white list first to kick off LSAT creation.
	_, _ = s.auctioneer.OrderState(ctx, order.Nonce{})

	// Prepare the keys we are going to try. Possibly not all of them will
	// be used.
	acctKeys, err := account.GenerateRecoveryKeys(
		ctx, target, s.lndServices.WalletKit,
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

	return recoveredAccounts, nil
}

// fullClientRecovery executes the account recovery process.
func (s *rpcServer) fullClientRecovery(ctx context.Context,
	req *poolrpc.RecoverAccountsRequest) ([]*account.Account, error) {

	lndTxs, err := s.lndServices.Client.ListTransactions(ctx, 0, -1)
	if err != nil {
		return nil, fmt.Errorf("unable to get txs from lnd: %v", err)
	}

	// Reverse transaction order, so we have older ones at the beginning.
	for i, j := 0, len(lndTxs)-1; i < j; i, j = i+1, j-1 {
		lndTxs[i], lndTxs[j] = lndTxs[j], lndTxs[i]
	}

	txs := make([]*wire.MsgTx, 0, len(lndTxs))
	for _, tx := range lndTxs {
		txs = append(txs, tx.Tx)
	}

	key, fstBlock := account.GetAuctioneerData(s.server.cfg.Network)
	if req.AuctioneerKey == "" {
		req.AuctioneerKey = key
	}
	if req.HeightHint == 0 {
		req.HeightHint = fstBlock
	}

	if req.AuctioneerKey == "" || req.HeightHint == 0 {
		return nil, fmt.Errorf("unable to get auctioner data")
	}

	auctioneerPubKey, err := account.DecodeAndParseKey(req.AuctioneerKey)
	if err != nil {
		return nil, fmt.Errorf("unable to decode an parse key: %v", err)
	}
	batchKey, err := account.DecodeAndParseKey(account.InitialBatchKey)
	if err != nil {
		return nil, fmt.Errorf("unable to decode an parse key: %v", err)
	}

	cfg := account.RecoveryConfig{
		Network:       s.server.cfg.Network,
		AccountTarget: req.AccountTarget,
		FirstBlock:    req.HeightHint,
		BitcoinConfig: &account.BitcoinConfig{
			Host:         req.BitcoinHost,
			User:         req.BitcoinUser,
			Password:     req.BitcoinPassword,
			HTTPPostMode: req.BitcoinHttppostmode,
			UseTLS:       req.BitcoinUsetls,
			TLSPath:      req.BitcoinTlspath,
		},
		Transactions:     txs,
		Signer:           s.lndServices.Signer,
		Wallet:           s.lndServices.WalletKit,
		InitialBatchKey:  batchKey,
		AuctioneerPubKey: auctioneerPubKey,
		Quit:             s.quit,
	}

	recoveredAccounts, err := account.RecoverAccounts(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("error performing recovery: %v", err)
	}

	return recoveredAccounts, nil
}

func (s *rpcServer) RecoverAccounts(ctx context.Context,
	req *poolrpc.RecoverAccountsRequest) (*poolrpc.RecoverAccountsResponse,
	error) {

	s.recoveryMutex.Lock()
	if s.recoveryPending {
		s.recoveryMutex.Unlock()
		return nil, fmt.Errorf("recovery already in progress")
	}
	s.recoveryPending = true
	s.recoveryMutex.Unlock()

	// Mark the recovery process as done whenever we finish.
	defer func() {
		s.recoveryMutex.Lock()
		s.recoveryPending = false
		s.recoveryMutex.Unlock()
	}()

	log.Infof("Attempting to recover accounts...")

	var recoveredAccounts []*account.Account
	var err error

	target := req.AccountTarget
	if target == 0 {
		target = account.DefaultAccountKeyWindow
	}

	if !req.FullClient {
		recoveredAccounts, err = s.serverAssistedRecovery(ctx, target)
	} else {
		recoveredAccounts, err = s.fullClientRecovery(ctx, req)
	}
	if err != nil {
		return nil, err
	}

	// Store the recovered accounts now and start watching them. If anything
	// went wrong during recovery before, no account is stored yet. This is
	// nice since it allows us to try recovery multiple times until it
	// actually works.
	numRecovered := len(recoveredAccounts)
	var maxIndex uint32
	for _, acct := range recoveredAccounts {
		// We need to know the highest index we ever used so we can make
		// sure lnd's wallet is also at that index and the next account
		// will be derived from the proper index.
		if acct.TraderKey.Index > maxIndex {
			maxIndex = acct.TraderKey.Index
		}

		err = s.accountManager.RecoverAccount(ctx, acct)
		if err != nil {
			// If something goes wrong for one account we still want
			// to continue with the others.
			numRecovered--
			rpcLog.Errorf("Error storing recovered account: %v", err)
		}
	}

	// Try to ratchet forward lnd's derivation index for accounts.
	err = account.AdvanceAccountDerivationIndex(
		ctx, maxIndex, s.lndServices.WalletKit,
		s.lndServices.ChainParams,
	)
	if err != nil {
		rpcLog.Errorf("Error advancing lnd's wallet to index %d: %v",
			maxIndex, err)
	}

	return &poolrpc.RecoverAccountsResponse{
		NumRecoveredAccounts: uint32(numRecovered),
	}, nil
}

// assertAccountReady enrues that an account is in the "ready" state that
// allows it to submit orders.We'll only allow orders for accounts that present
// in an open state, or have a pending update or batch. On the server-side if
// we have a pending update we won't be matched, but this lets us place our
// orders early so we can join the earliest available batch.
func assertAccountReady(acct *account.Account) error {
	switch acct.State {
	case account.StateOpen, account.StatePendingUpdate,
		account.StatePendingBatch:

		return nil

	default:
		return fmt.Errorf("acct=%x is in state %v, cannot "+
			"make order",
			acct.TraderKey.PubKey.SerializeCompressed(), acct.State)
	}
}

// validateOrder validates the order to ensure that all fields are consistent,
// and the order is likely to be accepted by the auctioneer. If this method
// returns nil, then the order is safe to submit to the auctioneer.
func (s *rpcServer) validateOrder(o order.Order, acct *account.Account,
	auctionTerms *terms.AuctioneerTerms) error {

	batchVersion, err := s.server.determineBatchVersion()
	if err != nil {
		return err
	}

	var usesAnchors bool
	switch o.Details().ChannelType {
	case order.ChannelTypeScriptEnforced:
		usesAnchors = true
	default:
		usesAnchors = false
	}

	switch castOrder := o.(type) {
	case *order.Ask:
		if castOrder.ConfirmationConstraints == order.OnlyZeroConf {
			if !batchVersion.SupportsZeroConfChannels() {
				return fmt.Errorf("batch version %v does not "+
					"support zero conf channels",
					batchVersion)
			}

			if !usesAnchors {
				return fmt.Errorf("zero conf channels need " +
					"to use anchors. Use the script " +
					"enforced channel type")
			}
		}

		if castOrder.AuctionType == order.BTCOutboundLiquidity &&
			castOrder.MinUnitsMatch == 0 {

			return fmt.Errorf("min chan amount must be greater " +
				"than zero for participating in the outbound " +
				"liquidity market")
		}

	case *order.Bid:
		if castOrder.ZeroConfChannel {
			if !batchVersion.SupportsZeroConfChannels() {
				return fmt.Errorf("batch version %v does not "+
					"support zero conf channels",
					batchVersion)
			}

			if !usesAnchors {
				return fmt.Errorf("zero conf channels need " +
					"to use anchors. Use the script " +
					"enforced channel type")
			}
		}
	}

	// Now that we now how large the order is, ensure that if it's a
	// wumbo-sized order, then the backing lnd node is advertising wumbo
	// support.
	if o.Details().Amt > lndFunding.MaxBtcFundingAmount &&
		!s.wumboSupported {

		return fmt.Errorf("%v is wumbo sized, but "+
			"lnd node isn't signalling wumbo", o.Details().Amt)
	}

	// Ensure that the account can actually submit orders in its present
	// state.
	if err := assertAccountReady(acct); err != nil {
		return err
	}

	// If the market isn't currently accepting orders for this particular
	// lease duration, then we'll exit here as the order will be rejected.
	leaseDuration := o.Details().LeaseDuration
	if _, ok := auctionTerms.LeaseDurationBuckets[leaseDuration]; !ok {
		return fmt.Errorf("invalid channel lease duration %v "+
			"blocks, active durations are: %v",
			leaseDuration, auctionTerms.LeaseDurationBuckets)
	}

	// If the order does not specify any AllowedNodeIDs/NotAllowedNodeIDs
	// it means that it can match with any other order. However, both
	// fields cannot be set at the same time.
	if len(o.Details().AllowedNodeIDs) > 0 &&
		len(o.Details().NotAllowedNodeIDs) > 0 {

		return fmt.Errorf("allowed and not allowed node ids set at " +
			"the same time")
	}

	if o.Details().AuctionType == order.BTCOutboundLiquidity &&
		o.Details().Units != 1 {

		return fmt.Errorf("to participate in the outbound liquidity " +
			"market the order amt should be exactly 100k sats")
	}

	return nil
}

// orderPreparer represents a type of function that inserts the order into the
// local database, and returns the params needed to submit it to the
// auctioneer.
type orderPreparer func(context.Context, order.Order,
	*account.Account, *terms.AuctioneerTerms) (*order.ServerOrderParams,
	error)

// prepareAndSubmitOrder performs a series of final checks locally to ensure
// the order is valid, before submitting it to the auctioneer.
func prepareAndSubmitOrder(ctx context.Context, o order.Order,
	auctionTerms *terms.AuctioneerTerms, acct *account.Account,
	auction *auctioneer.Client, prepareOrder orderPreparer) error {

	// Collect all the order data and sign it before sending it to the
	// auction server.
	serverParams, err := prepareOrder(
		ctx, o, acct, auctionTerms,
	)
	if err != nil {
		return err
	}

	// Send the order to the server. If this fails, then the order is
	// certain to never get into the order book. We don't need to keep it
	// around in that case.
	//
	// TODO(roasbeef): commit initiator to disk so don't lose when
	// submitting orders for sidecar channels?
	return auction.SubmitOrder(
		ctx, o, serverParams,
	)
}

// SubmitOrder assembles all the information that is required to submit an order
// from the trader's lnd node, signs it and then sends the order to the server
// to be included in the auctioneer's order book.
func (s *rpcServer) SubmitOrder(ctx context.Context,
	req *poolrpc.SubmitOrderRequest) (*poolrpc.SubmitOrderResponse, error) {

	// Guard this RPC from being used before we've started up fully.
	if s.server.lndServices == nil {
		return nil, fmt.Errorf("cannot submit order, trader daemon " +
			"still starting up")
	}

	// We'll use this channel type selector to pick a channel type based on
	// the current connected lnd version. This lets us graceful update to
	// new features as they're available, while still supporting older
	// versions of lnd.
	chanTypeSelector := order.WithDefaultChannelType(func() order.ChannelType {
		// TODO(guggero): Use order.ChannelTypeScriptEnforced here as
		// soon as a large percentage of ask orders support script
		// enforced channel types to not cause a market segregation for
		// lnd 0.14.x or later users without them being aware of why
		// their bids aren't getting matched.
		return order.ChannelTypePeerDependent
	})

	var o order.Order
	switch requestOrder := req.Details.(type) {
	case *poolrpc.SubmitOrderRequest_Ask:
		a := requestOrder.Ask
		kit, err := order.ParseRPCOrder(
			a.Version, a.LeaseDurationBlocks, a.Details,
			chanTypeSelector,
		)
		if err != nil {
			return nil, err
		}

		announcement := order.ChannelAnnouncementConstraints(
			a.AnnouncementConstraints,
		)
		confirmations := order.ChannelConfirmationConstraints(
			a.ConfirmationConstraints,
		)
		o = &order.Ask{
			Kit:                     *kit,
			AnnouncementConstraints: announcement,
			ConfirmationConstraints: confirmations,
		}

	case *poolrpc.SubmitOrderRequest_Bid:
		b := requestOrder.Bid
		kit, err := order.ParseRPCOrder(
			b.Version, b.LeaseDurationBlocks, b.Details,
			chanTypeSelector,
		)
		if err != nil {
			return nil, err
		}
		nodeTier, err := unmarshallNodeTier(b.MinNodeTier)
		if err != nil {
			return nil, err
		}
		ticket, err := unmarshallSidecar(kit.Version, b.SidecarTicket)
		if err != nil {
			return nil, err
		}

		o = &order.Bid{
			Kit:                *kit,
			MinNodeTier:        nodeTier,
			SelfChanBalance:    btcutil.Amount(b.SelfChanBalance),
			SidecarTicket:      ticket,
			UnannouncedChannel: b.UnannouncedChannel,
			ZeroConfChannel:    b.ZeroConfChannel,
		}

	default:
		return nil, fmt.Errorf("invalid order request")
	}

	// We also need to know the current maximum order duration.
	auctionTerms, err := s.auctioneer.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not query auctioneer terms: %v",
			err)
	}

	// Verify that the account exists.
	acctKey, err := btcec.ParsePubKey(o.Details().AcctKey[:])
	if err != nil {
		return nil, err
	}
	acct, err := s.server.db.Account(acctKey)
	if err != nil {
		return nil, fmt.Errorf("cannot accept order: %v", err)
	}

	// Validate the order to ensure the account is in a live state, and the
	// target lease duration period actually exists.
	if err := s.validateOrder(o, acct, auctionTerms); err != nil {
		return nil, fmt.Errorf("order valid validation: %w", err)
	}

	// Finally add the order to the local order database, and submit it to
	// the auctioneer server.
	err = prepareAndSubmitOrder(
		ContextWithInitiator(ctx, req.Initiator), o, auctionTerms,
		acct, s.auctioneer, s.orderManager.PrepareOrder,
	)
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

	// We should update the sidecar ticket if this was a bid created for it.
	// The ticket in the order struct was already updated with the order
	// information and signature, but it was only stored as TLV data with
	// the order itself. We should also update the ticket in the main
	// sidecar ticket bucket.
	var encodedTicket string
	bid, isBid := o.(*order.Bid)
	if isBid && bid.SidecarTicket != nil {
		err := s.server.db.UpdateSidecar(bid.SidecarTicket)
		if err != nil {
			return nil, fmt.Errorf("error updating sidecar "+
				"ticket: %v", err)
		}

		encodedTicket, err = sidecar.EncodeToString(bid.SidecarTicket)
		if err != nil {
			return nil, fmt.Errorf("error encoding sidecar "+
				"ticket: %v", err)
		}
	}

	// ServerOrder is accepted.
	orderNonce := o.Nonce()
	return &poolrpc.SubmitOrderResponse{
		Details: &poolrpc.SubmitOrderResponse_AcceptedOrderNonce{
			AcceptedOrderNonce: orderNonce[:],
		},
		UpdatedSidecarTicket: encodedTicket,
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

	// We also need a map of all account versions for the locked value
	// estimation below.
	accountVersions := make(map[[33]byte]account.Version)
	allAccounts, err := s.server.db.Accounts()
	if err != nil {
		return nil, fmt.Errorf("error querying accounts: %v", err)
	}
	for _, acct := range allAccounts {
		var rawKey [33]byte
		copy(rawKey[:], acct.TraderKey.PubKey.SerializeCompressed())
		accountVersions[rawKey] = acct.Version
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

		allowedNodeIDs := order.MarshalNodeIDSlice(
			dbOrder.Details().AllowedNodeIDs,
		)

		notAllowedNodeIDs := order.MarshalNodeIDSlice(
			dbOrder.Details().NotAllowedNodeIDs,
		)

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
			ReservedValueSat: uint64(dbOrder.ReservedValue(
				feeSchedule, accountVersions[dbDetails.AcctKey],
			)),
			CreationTimestampNs: uint64(evt.Timestamp().UnixNano()),
			Events:              rpcEvents,
			MinUnitsMatch:       uint32(dbOrder.Details().MinUnitsMatch),
			ChannelType: marshallChannelType(
				dbOrder.Details().ChannelType,
			),
			AuctionType: auctioneerrpc.AuctionType(
				dbOrder.Details().AuctionType,
			),
			AllowedNodeIds:    allowedNodeIDs,
			NotAllowedNodeIds: notAllowedNodeIDs,
			IsPublic:          dbOrder.Details().IsPublic,
		}

		switch o := dbOrder.(type) {
		case *order.Ask:
			announcement := auctioneerrpc.ChannelAnnouncementConstraints(
				o.AnnouncementConstraints,
			)
			confirmations := auctioneerrpc.ChannelConfirmationConstraints(
				o.ConfirmationConstraints,
			)
			rpcAsk := &poolrpc.Ask{
				Details:                 details,
				LeaseDurationBlocks:     dbDetails.LeaseDuration,
				Version:                 uint32(o.Version),
				AnnouncementConstraints: announcement,
				ConfirmationConstraints: confirmations,
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
				SelfChanBalance:     uint64(o.SelfChanBalance),
				UnannouncedChannel:  o.UnannouncedChannel,
				ZeroConfChannel:     o.ZeroConfChannel,
			}

			// The sidecar ticket was given to the auction server
			// and stored as the raw tlv stream. We only want the
			// user to see the more human readable base64 encoded
			// version of the ticket though. That's why we string
			// encode it here instead of sending the raw bytes back.
			// If we did bytes it would be shown hex encoded and not
			// base64 with the nice prefix.
			if o.SidecarTicket != nil {
				rpcBid.SidecarTicket, err = sidecar.EncodeToString(
					o.SidecarTicket,
				)
				if err != nil {
					return nil, fmt.Errorf("error "+
						"encoding sidecar ticket: %v",
						err)
				}
			}

			bids = append(bids, rpcBid)

		default:
			return nil, fmt.Errorf("unknown order type: %v", o)
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

	// If this order was for a sidecar ticket, we also want to cancel the
	// ticket itself since it will never be completed anyway.
	err = s.setTicketStateForOrder(sidecar.StateCanceled, nonce)
	if err != nil {
		return nil, fmt.Errorf("error updating our sidecar ticket "+
			"after canceling order: %v", err)
	}

	return &poolrpc.CancelOrderResponse{}, nil
}

// QuoteOrder calculates the premium, execution fees and max batch fee rate for
// an order based on the given order parameters.
func (s *rpcServer) QuoteOrder(ctx context.Context,
	req *poolrpc.QuoteOrderRequest) (*poolrpc.QuoteOrderResponse, error) {

	if req.MinUnitsMatch == 0 {
		return nil, fmt.Errorf("min units match must be specified")
	}

	auctionTerms, err := s.auctioneer.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to query auctioneer terms: %v",
			err)
	}

	feeSchedule := terms.NewLinearFeeSchedule(
		auctionTerms.OrderExecBaseFee,
		auctionTerms.OrderExecFeeRate,
	)

	q := order.NewQuote(
		btcutil.Amount(req.Amt),
		order.SupplyUnit(req.MinUnitsMatch).ToSatoshis(),
		order.FixedRatePremium(req.RateFixed), req.LeaseDurationBlocks,
		chainfee.SatPerKWeight(req.MaxBatchFeeRateSatPerKw),
		feeSchedule,
	)

	return &poolrpc.QuoteOrderResponse{
		TotalPremiumSat:      uint64(q.TotalPremium),
		RatePerBlock:         q.RatePerBlock,
		RatePercent:          q.RatePerBlock * 100,
		TotalExecutionFeeSat: uint64(q.TotalExecutionFee),
		WorstCaseChainFeeSat: uint64(q.WorstCaseChainFee),
	}, nil
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

	msg := &auctioneerrpc.ClientAuctionMessage_Reject{
		Reject: &auctioneerrpc.OrderMatchReject{
			BatchId: batch.ID[:],
			Reason:  failure.Error(),
		},
	}

	// Attach the status code to the message to give a bit more context.
	var (
		partialReject   *funding.MatchRejectErr
		versionMismatch *order.ErrVersionMismatch
	)
	switch {
	case errors.As(failure, &versionMismatch):
		msg.Reject.ReasonCode = auctioneerrpc.OrderMatchReject_BATCH_VERSION_MISMATCH

		// Track this reject by adding an event to our orders.
		err := s.server.db.StoreBatchEvents(
			batch, order.MatchStateRejected,
			poolrpc.MatchRejectReason_BATCH_VERSION_MISMATCH,
		)
		if err != nil {
			rpcLog.Errorf("Could not store match event: %v", err)
		}

	case errors.Is(failure, order.ErrMismatchErr):
		msg.Reject.ReasonCode = auctioneerrpc.OrderMatchReject_SERVER_MISBEHAVIOR

		// Track this reject by adding an event to our orders.
		err := s.server.db.StoreBatchEvents(
			batch, order.MatchStateRejected,
			poolrpc.MatchRejectReason_SERVER_MISBEHAVIOR,
		)
		if err != nil {
			rpcLog.Errorf("Could not store match event: %v", err)
		}

	case errors.As(failure, &partialReject):
		msg.Reject.ReasonCode = auctioneerrpc.OrderMatchReject_PARTIAL_REJECT
		msg.Reject.RejectedOrders = make(map[string]*auctioneerrpc.OrderReject)
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
		msg.Reject.ReasonCode = auctioneerrpc.OrderMatchReject_UNKNOWN
	}

	rpcLog.Infof("Sending batch rejection message for batch %x with "+
		"code %v and message: %v", batch.ID, msg.Reject.ReasonCode,
		failure)

	// Send the message to the server. If a new error happens we return that
	// one because we know the causing error has at least been logged at
	// some point before.
	err = s.auctioneer.SendAuctionMessage(&auctioneerrpc.ClientAuctionMessage{
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
	return s.auctioneer.SendAuctionMessage(&auctioneerrpc.ClientAuctionMessage{
		Msg: &auctioneerrpc.ClientAuctionMessage_Accept{
			Accept: &auctioneerrpc.OrderMatchAccept{
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
	nonces order.AccountNonces,
	chanInfos map[wire.OutPoint]*chaninfo.ChannelInfo) error {

	// Prepare the list of witness stacks and channel infos and send them to
	// the server.
	rpcSigs := make(map[string][]byte, len(sigs))
	for acctKey, sig := range sigs {
		key := hex.EncodeToString(acctKey[:])
		rpcSigs[key] = make([]byte, len(sig))
		copy(rpcSigs[key], sig)
	}

	// Prepare the trader's nonces too (if there are any Taproot/MuSig2
	// accounts in the batch).
	rpcNonces := make(map[string][]byte, len(nonces))
	for acctKey, nonce := range nonces {
		key := hex.EncodeToString(acctKey[:])
		rpcNonces[key] = make([]byte, 66)
		copy(rpcNonces[key], nonce[:])
	}

	rpcChannelInfos, err := marshallChannelInfo(chanInfos)
	if err != nil {
		return fmt.Errorf("error marshalling channel info: %v", err)
	}

	rpcLog.Infof("Sending OrderMatchSign for batch %x", batch.ID[:])

	// We've successfully processed the sign message, let's store an event
	// for this for all orders that were involved on our side.
	if err := s.server.db.StoreBatchEvents(
		batch, order.MatchStateSigned, poolrpc.MatchRejectReason_NONE,
	); err != nil {
		rpcLog.Errorf("Unable to store order events: %v", err)
	}

	return s.auctioneer.SendAuctionMessage(&auctioneerrpc.ClientAuctionMessage{
		Msg: &auctioneerrpc.ClientAuctionMessage_Sign{
			Sign: &auctioneerrpc.OrderMatchSign{
				BatchId:      batch.ID[:],
				AccountSigs:  rpcSigs,
				TraderNonces: rpcNonces,
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

	return &poolrpc.AuctionFeeResponse{
		ExecutionFee: &auctioneerrpc.ExecutionFee{
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
		_, err := btcec.ParsePubKey(rawAccountKey)
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
			batchKey, err := btcec.ParsePubKey(rawBatchID)
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
		switch height := channel.ThawHeight; {
		case height == 0:
			continue

		case height < 500_000:
			// If the value is lower than 500,000, then it's
			// interpreted as a relative height and we need to add
			// an offset from the confirmation height of the
			// channel.
			sid := lnwire.NewShortChanIDFromInt(channel.ChanId)
			expiryHeight := height + sid.BlockHeight
			chanLeaseExpiries[channel.ChannelPoint] = expiryHeight

		default:
			// If the value is greater than 500,000, it is
			// interpreted as an absolute value.
			chanLeaseExpiries[channel.ChannelPoint] = height
		}
	}

	return s.prepareLeasesResponse(
		ctx, accounts, batches, chanLeaseExpiries,
	)
}

// fetchNodeRatings returns an up to date set of ratings for each of the nodes
// we matched with in the passed batch.
func fetchNodeRatings(ctx context.Context, batches []*clientdb.LocalBatchSnapshot,
	auctioneer *auctioneer.Client) (map[[33]byte]auctioneerrpc.NodeTier,
	error) {

	// As the node ratings call is batched, we'll first do a single query
	// to fetch all the ratings that we'll want to populate below.
	//
	// TODO(roasbeef): capture rating at time of lease? also cache
	nodeRatings := make(map[[33]byte]auctioneerrpc.NodeTier)
	for _, batch := range batches {
		for _, matches := range batch.MatchedOrders {
			for _, match := range matches {
				nodeRatings[match.NodeKey] = 0
			}
		}
	}

	nodeKeys := make([]*btcec.PublicKey, 0, len(nodeRatings))
	for pubKey := range nodeRatings {
		nodeKey, err := btcec.ParsePubKey(pubKey[:])
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
				ourMultiSigPubKey := ourMultiSigKey.PubKey

				// For sidecar orders the multisig key used
				// isn't our own.
				isSidecarChannel := false
				ourOrderBid, ourOrderIsBid := ourOrder.(*order.Bid)
				if ourOrderIsBid && ourOrderBid.SidecarTicket != nil {
					t := ourOrderBid.SidecarTicket
					if t.Recipient == nil {
						continue
					}

					isSidecarChannel = true
					ourMultiSigPubKey = t.Recipient.MultiSigPubKey
				}

				chanAmt := match.UnitsFilled.ToSatoshis()
				_, chanOutput, err := input.GenFundingPkScript(
					ourMultiSigPubKey.SerializeCompressed(),
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
				var (
					bidDuration     uint32
					selfChanBalance uint64
				)
				if ourOrder.Type() == order.TypeBid {
					bidDuration = ourOrder.(*order.Bid).LeaseDuration
					selfChanBalance = uint64(
						ourOrder.(*order.Bid).SelfChanBalance,
					)
				} else {
					bidDuration = match.Order.(*order.Bid).LeaseDuration
				}

				// The clearing price is dependent on the lease
				// duration, except for older batch versions
				// where there was just one single duration
				// which is returned in the default/legacy
				// bucket independent of what bids were
				// contained within.
				duration := bidDuration
				clearingPrice, ok := batch.ClearingPrices[duration]
				if !ok {
					duration = order.LegacyLeaseDurationBucket
					clearingPrice = batch.ClearingPrices[duration]
				}

				premiumAmt := chanAmt
				auctionType := ourOrder.Details().AuctionType
				if auctionType == order.BTCOutboundLiquidity {
					premiumAmt += btcutil.Amount(
						selfChanBalance,
					)
				}

				// Calculate the premium paid/received to/from
				// the maker/taker and the execution fee paid
				// to the auctioneer and tally them.
				premium := clearingPrice.LumpSumPremium(
					premiumAmt, bidDuration,
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
				matchedNonce := match.Order.Nonce()

				rpcLeases = append(rpcLeases, &poolrpc.Lease{
					ChannelPoint: &auctioneerrpc.OutPoint{
						Txid:        batchTxHash[:],
						OutputIndex: chanOutputIdx,
					},
					ChannelAmtSat:         uint64(chanAmt),
					ChannelDurationBlocks: bidDuration,
					ChannelLeaseExpiry:    leaseExpiryHeight,
					PremiumSat:            uint64(premium),
					ClearingRatePrice:     uint64(clearingPrice),
					OrderFixedRate: uint64(
						ourOrder.Details().FixedRate,
					),
					ExecutionFeeSat:      uint64(exeFee),
					OrderNonce:           nonce[:],
					MatchedOrderNonce:    matchedNonce[:],
					Purchased:            purchased,
					ChannelRemoteNodeKey: match.NodeKey[:],
					ChannelNodeTier:      nodeTier,
					SelfChanBalance:      selfChanBalance,
					SidecarChannel:       isSidecarChannel,
				})

				numChans++
			}

			// Estimate the chain fees paid for the number of
			// channels created in this batch and tally them.
			//
			// TODO(guggero): This is just an approximation! We
			// should properly calculate the fees _per account_ as
			// that's what we do on the server side. Then we can
			// also take a look at the actual account version at the
			// time of the batch.
			chainFee := order.EstimateTraderFee(
				uint32(numChans), batch.BatchTxFeeRate,
				account.VersionInitialNoVersion,
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
	req *auctioneerrpc.BatchSnapshotRequest) (
	*auctioneerrpc.BatchSnapshotResponse, error) {

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
	req *auctioneerrpc.BatchSnapshotsRequest) (
	*auctioneerrpc.BatchSnapshotsResponse, error) {

	// To allow for easy querying through REST, we use the fallback value of
	// showing one batch if no number is specified.
	if req.NumBatchesBack == 0 {
		req.NumBatchesBack = 1
	}

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
	legacyDurations := make(
		map[uint32]bool, len(auctionTerms.LeaseDurationBuckets),
	)
	for duration, market := range auctionTerms.LeaseDurationBuckets {
		const accept = auctioneerrpc.DurationBucketState_ACCEPTING_ORDERS
		const open = auctioneerrpc.DurationBucketState_MARKET_OPEN

		legacyDurations[duration] = market == accept || market == open
	}

	return &poolrpc.LeaseDurationResponse{
		LeaseDurations:       legacyDurations,
		LeaseDurationBuckets: auctionTerms.LeaseDurationBuckets,
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
		ConfTarget:               auctionTerms.NextBatchConfTarget,
		FeeRateSatPerKw:          uint64(auctionTerms.NextBatchFeeRate),
		ClearTimestamp:           uint64(auctionTerms.NextBatchClear.Unix()),
		AutoRenewExtensionBlocks: auctionTerms.AutoRenewExtensionBlocks,
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
		nodeKey, err := btcec.ParsePubKey(nodeKeyBytes)
		if err != nil {
			return nil, err
		}

		pubKeys = append(pubKeys, nodeKey)
	}

	return s.auctioneer.NodeRating(ctx, pubKeys...)
}

// GetInfo returns general information about the state of the Pool trader
// daemon.
func (s *rpcServer) GetInfo(ctx context.Context,
	_ *poolrpc.GetInfoRequest) (*poolrpc.GetInfoResponse, error) {

	info := &poolrpc.GetInfoResponse{
		Version:                Version(),
		CurrentBlockHeight:     atomic.LoadUint32(&s.bestHeight),
		SubscribedToAuctioneer: s.auctioneer.IsSubscribed(),
		NewNodesOnly:           s.server.cfg.NewNodesOnly,
	}

	// Query our own node's rating. We want the GetInfo call to be available
	// in an offline situation to simplify debugging. Therefore if there's
	// an error getting our node rating from the auctioneer, we just log a
	// warning and the rating will be returned as nil.
	nodePubkeyRaw, err := s.orderManager.OurNodePubkey()
	if err != nil {
		return nil, fmt.Errorf("cannot get our node pubkey: %v", err)
	}
	nodePubkey, err := btcec.ParsePubKey(nodePubkeyRaw[:])
	if err != nil {
		return nil, fmt.Errorf("cannot parse our node pubkey: %v", err)
	}
	ratings, err := s.auctioneer.NodeRating(ctx, nodePubkey)
	if err != nil {
		rpcLog.Warnf("Error querying node rating from auctioneer: %v",
			err)
	} else {
		// If we do get a response, we expect exactly one rating though.
		if len(ratings.NodeRatings) != 1 {
			return nil, fmt.Errorf("unexpected number of node "+
				"ratings %d", len(ratings.NodeRatings))
		}
		info.NodeRating = ratings.NodeRatings[0]
	}

	// The same goes for the market info, we don't fail if the call to the
	// server fails.
	marketInfo, err := s.auctioneer.MarketInfo(ctx)
	if err != nil {
		rpcLog.Warnf("Error querying market info from auctioneer: %v",
			err)
	} else {
		info.MarketInfo = marketInfo.Markets
	}

	// Tally up account statistics.
	accounts, err := s.server.db.Accounts()
	if err != nil {
		return nil, fmt.Errorf("error loading accounts: %v", err)
	}
	for _, acct := range accounts {
		info.AccountsTotal++

		if acct.State.IsActive() {
			info.AccountsActive++

			if acct.State == account.StateExpired {
				info.AccountsActiveExpired++
			}
		} else {
			info.AccountsArchived++
		}
	}

	// Tally up order statistics.
	orders, err := s.server.db.GetOrders()
	if err != nil {
		return nil, fmt.Errorf("error loading orders: %v", err)
	}
	for _, o := range orders {
		info.OrdersTotal++

		if o.Details().State.Archived() {
			info.OrdersArchived++
		} else {
			info.OrdersActive++
		}
	}

	// Count the number of local batch snapshots which should be equivalent
	// to the number of batches a local account was involved in.
	batches, err := s.server.db.GetLocalBatchSnapshots()
	if err != nil {
		return nil, fmt.Errorf("error loading batches: %v", err)
	}
	info.BatchesInvolved = uint32(len(batches))

	// Finally count the number of LSAT tokens in our store.
	tokens, err := s.server.lsatStore.AllTokens()
	if err != nil {
		return nil, fmt.Errorf("error loading tokens: %v", err)
	}
	info.LsatTokens = uint32(len(tokens))

	return info, nil
}

// StopDaemon gracefully shuts down the Pool trader daemon.
func (s *rpcServer) StopDaemon(_ context.Context,
	_ *poolrpc.StopDaemonRequest) (*poolrpc.StopDaemonResponse, error) {

	rpcLog.Infof("Stop requested through RPC, gracefully shutting down")
	s.server.cfg.RequestShutdown()

	return &poolrpc.StopDaemonResponse{}, nil
}

// OfferSidecar is step 1/4 of the sidecar negotiation between the provider
// (the trader submitting the bid order) and the recipient (the trader
// receiving the sidecar channel).
// This step must be run by the provider. The result is a sidecar ticket with
// an offer to lease a sidecar channel for the recipient. The offer will be
// signed with the provider's lnd node public key. The ticket returned by this
// call will have the state "offered".
func (s *rpcServer) OfferSidecar(ctx context.Context,
	req *poolrpc.OfferSidecarRequest) (*poolrpc.SidecarTicket, error) {

	// Do some basic sanity checks first.
	switch {
	case req.Bid == nil:
		return nil, fmt.Errorf("bid must be set")

	case req.Bid.Details.Amt == 0:
		return nil, fmt.Errorf("channel capacity missing")
	}

	// We'll need to look up the account state in the database to make sure
	// the account is actually still open (able to submit bids), and also
	// to grab the KeyDescriptor that we'll need for signing later.
	acctKey, err := btcec.ParsePubKey(req.Bid.Details.TraderKey)
	if err != nil {
		return nil, err
	}
	acct, err := s.server.db.Account(acctKey)
	if err != nil {
		return nil, err
	}

	// We always need to populate these fields because they may be used
	// to create a valid channel acceptor.
	bid := &order.Bid{
		UnannouncedChannel: req.Bid.UnannouncedChannel,
		ZeroConfChannel:    req.Bid.ZeroConfChannel,
	}

	// If automated negotiation was set, then we'll parse out the rest of
	// the bid now so we can validate that it'll pass all checks when we
	// eventually need to submit it.
	if req.AutoNegotiate {
		kit, err := order.ParseRPCOrder(
			req.Bid.Version, req.Bid.LeaseDurationBlocks,
			req.Bid.Details,
		)
		if err != nil {
			return nil, err
		}
		nodeTier, err := unmarshallNodeTier(req.Bid.MinNodeTier)
		if err != nil {
			return nil, err
		}

		// We don't add the ticket here yet as we'll only add it at the
		// very end once the ticket has advanced to the final stage.
		bid = &order.Bid{
			Kit:                *kit,
			MinNodeTier:        nodeTier,
			SelfChanBalance:    btcutil.Amount(req.Bid.SelfChanBalance),
			UnannouncedChannel: req.Bid.UnannouncedChannel,
			ZeroConfChannel:    req.Bid.ZeroConfChannel,
		}

		// Perform some initial validation on the order to ensure that
		// we'll be able to eventually submit it.
		auctionTerms, err := s.auctioneer.Terms(ctx)
		if err != nil {
			return nil, fmt.Errorf("unable to query auctioneer "+
				"terms: %v", err)
		}
		err = s.validateOrder(bid, acct, auctionTerms)
		if err != nil {
			return nil, err
		}
	}

	// The funding manager does all the work, including signing and storing
	// the new ticket.
	ticket, err := s.server.fundingManager.OfferSidecar(
		ctx, btcutil.Amount(req.Bid.Details.Amt),
		btcutil.Amount(req.Bid.SelfChanBalance),
		req.Bid.LeaseDurationBlocks, acct.TraderKey, bid,
		req.AutoNegotiate,
	)
	if err != nil {
		return nil, err
	}

	var nonce order.Nonce
	if bid != nil {
		nonce = bid.Nonce()
	}

	// If the bid has already been specified, then we can go ahead and set
	// it within the ticket.
	ticket.Order = &sidecar.Order{
		BidNonce: nonce,
	}

	// If the ticket has requested automated negotiation, then we'll hand
	// it off to the coordinate tor now.
	if ticket.Offer.Auto {
		err := s.server.sidecarAcceptor.CoordinateSidecar(
			ticket, bid, acct,
		)
		if err != nil {
			return nil, err
		}
	}

	// We'll return a nice string encoded version of the ticket to the user.
	ticketStr, err := sidecar.EncodeToString(ticket)
	if err != nil {
		return nil, err
	}
	return &poolrpc.SidecarTicket{
		Ticket: ticketStr,
	}, nil
}

// RegisterSidecar is step 2/4 of the sidecar negotiation between the provider
// (the trader submitting the bid order) and the recipient (the trader receiving
// the sidecar channel).
// This step must be run by the recipient. The result is a sidecar ticket with
// the recipient's node information and channel funding multisig pubkey filled
// in. The ticket returned by this call will have the state "registered".
func (s *rpcServer) RegisterSidecar(ctx context.Context,
	req *poolrpc.RegisterSidecarRequest) (*poolrpc.SidecarTicket, error) {

	// Parse the ticket from its string encoded representation.
	ticket, err := sidecar.DecodeString(req.Ticket)
	if err != nil {
		return nil, fmt.Errorf("error decoding ticket: %v", err)
	}

	// The sidecar acceptor will add all required information and add the
	// ticket to our DB.
	registeredTicket, err := s.server.sidecarAcceptor.RegisterSidecar(
		ctx, *ticket,
	)
	if err != nil {
		return nil, err
	}

	// At this point, we'll now check if the ticket specifies that
	// automated negotiation is to be sued, if so then we'll hand things
	// off to the sidecar acceptor to finish the process.
	if registeredTicket.Offer.Auto {
		err := s.server.sidecarAcceptor.AutoAcceptSidecar(registeredTicket)
		if err != nil {
			return nil, fmt.Errorf("unable to start ticket auto "+
				"negotiation: %v", err)
		}
	}

	ticketStr, err := sidecar.EncodeToString(registeredTicket)
	if err != nil {
		return nil, err
	}
	return &poolrpc.SidecarTicket{Ticket: ticketStr}, nil
}

// expectSidecarChannel is a private version of ExpectSidecarChannel that may
// be used in earlier steps if automated negotiation is requested.
func (s *rpcServer) expectSidecarChannel(ctx context.Context,
	t *sidecar.Ticket) error {

	err := validateOrderedTicket(ctx, t, s.lndServices.Signer, s.server.db)
	if err != nil {
		return err
	}

	// Formally everything looks good so far. We can now pass the sidecar
	// order with the verified information to the acceptor and let it do its
	// job.
	return s.server.sidecarAcceptor.ExpectChannel(ctx, t)
}

// ExpectSidecarChannel is step 4/4 of the sidecar negotiation between the
// provider (the trader submitting the bid order) and the recipient (the trader
// receiving the sidecar channel).
// This step must be run by the recipient once the provider has submitted the
// bid order for the sidecar channel. From this point onwards the Pool trader
// daemon of both the provider as well as the recipient need to be online to
// receive and react to match making events from the server.
func (s *rpcServer) ExpectSidecarChannel(ctx context.Context,
	req *poolrpc.ExpectSidecarChannelRequest) (
	*poolrpc.ExpectSidecarChannelResponse, error) {

	// Parse the ticket from its string encoded representation.
	t, err := sidecar.DecodeString(req.Ticket)
	if err != nil {
		return nil, fmt.Errorf("error decoding ticket: %v", err)
	}

	if err := s.expectSidecarChannel(ctx, t); err != nil {
		return nil, fmt.Errorf("unable to expect sidecar chan: %v", err)
	}

	return &poolrpc.ExpectSidecarChannelResponse{}, nil
}

// DecodeSidecarTicket decodes the base58 encoded sidecar ticket into its
// individual data fields for a more human-readable representation.
func (s *rpcServer) DecodeSidecarTicket(ctx context.Context,
	req *poolrpc.SidecarTicket) (*poolrpc.DecodedSidecarTicket, error) {

	// Parse the ticket from its string encoded representation.
	ticket, err := sidecar.DecodeString(req.Ticket)
	if err != nil {
		return nil, fmt.Errorf("error decoding ticket: %v", err)
	}

	return marshallTicket(ticket), nil
}

// ListSidecars lists all sidecar tickets currently in the local database. This
// includes tickets offered by our node as well as tickets that our node is the
// recipient of. Optionally a ticket ID can be provided to filter the tickets.
func (s *rpcServer) ListSidecars(_ context.Context,
	req *poolrpc.ListSidecarsRequest) (*poolrpc.ListSidecarsResponse,
	error) {

	var (
		tickets []*sidecar.Ticket
		err     error
	)

	if len(req.SidecarId) > 0 {
		var id [8]byte
		copy(id[:], req.SidecarId)
		tickets, err = s.server.db.SidecarsByID(id)
	} else {
		tickets, err = s.server.db.Sidecars()
	}
	if err != nil {
		return nil, fmt.Errorf("error reading sidecar tickets: %v", err)
	}

	resp := &poolrpc.ListSidecarsResponse{
		Tickets: make([]*poolrpc.DecodedSidecarTicket, len(tickets)),
	}
	for idx, ticket := range tickets {
		resp.Tickets[idx] = marshallTicket(ticket)
	}

	return resp, nil
}

// CancelSidecar cancels the execution of a specific sidecar ticket. Depending
// on the state of the sidecar ticket its associated bid order might be
// canceled as well (if this ticket was offered by our node).
func (s *rpcServer) CancelSidecar(ctx context.Context,
	req *poolrpc.CancelSidecarRequest) (*poolrpc.CancelSidecarResponse,
	error) {

	if len(req.SidecarId) == 0 {
		return nil, fmt.Errorf("missing sidecar ID")
	}

	var id [8]byte
	copy(id[:], req.SidecarId)

	tickets, err := s.server.db.SidecarsByID(id)
	if err != nil {
		return nil, fmt.Errorf("error reading sidecar tickets: %v", err)
	}

	var ticket *sidecar.Ticket
	for _, t := range tickets {
		// If there is more than one ticket, let's pick the first one
		// that is not in a terminal state.
		if t.State.IsTerminal() {
			continue
		}

		ticket = t
		break
	}

	if ticket == nil {
		return nil, fmt.Errorf("no ticket with ID %x found or ticket "+
			"is already in terminal state", req.SidecarId)
	}

	// In case we are offering the ticket, we might also need to cancel the
	// order we created for it. This will also update the ticket state and
	// inform the sidecar acceptor. But those operations are idempotent, so
	// it doesn't matter. Canceling the order will make sure we aren't
	// getting matched again. In case the auctioneer doesn't accept order
	// cancellations at the moment (because it is currently processing a
	// batch), this operation will fail and will need to be repeated by the
	// user.
	if ticket.State >= sidecar.StateOrdered && ticket.Order != nil {
		_, err = s.CancelOrder(ctx, &poolrpc.CancelOrderRequest{
			OrderNonce: ticket.Order.BidNonce[:],
		})
		if err != nil {
			return nil, fmt.Errorf("error canceling order %x for "+
				"sidecar ticket %x: %v",
				ticket.Order.BidNonce[:], ticket.ID[:], err)
		}
	}

	// Before updating the ticket, we'll signal to the acceptor that the
	// channel that we were a provider of has been finalized so the state
	// machine can terminate. This is a no-op if the ticket isn't registered
	// with the acceptor because we are offering and not receiving it. If
	// we were offering it, this will move the ticket to "completed". That's
	// why we do it before updating the ticket again to reflect the cancel
	// state below.
	ticket.State = sidecar.StateCanceled
	s.server.sidecarAcceptor.FinalizeTicket(ticket)

	// Set the state to canceled and update our local database. This will
	// also delete any bid template in the sidecar bucket if it existed.
	ticket.State = sidecar.StateCanceled
	if err := s.server.db.UpdateSidecar(ticket); err != nil {
		return nil, fmt.Errorf("error updating sidecar ticket with ID "+
			"%x to state %d: %v", ticket.ID[:], ticket.State, err)
	}

	return &poolrpc.CancelSidecarResponse{}, nil
}

// setTicketStateForOrder updates the sidecar ticket state we have for a given
// order in our local database to the new state.
func (s *rpcServer) setTicketStateForOrder(newState sidecar.State,
	nonce order.Nonce) error {

	tickets, err := s.server.db.Sidecars()
	if err != nil {
		return fmt.Errorf("error reading sidecar tickets: %v", err)
	}

	for _, ticket := range tickets {
		if ticket.Order == nil || ticket.Order.BidNonce != nonce {
			continue
		}

		ticket.State = newState
		if err := s.server.db.UpdateSidecar(ticket); err != nil {
			return fmt.Errorf("error updating sidecar ticket "+
				"with ID %x to state %d: %v", ticket.ID[:],
				newState, err)
		}

		// In addition to updating the ticket, we'll also signal to the
		// acceptor that the channel that we were a provider of has
		// been finalized so the state machine can terminate.
		s.server.sidecarAcceptor.FinalizeTicket(ticket)
	}

	return nil
}

// determineAccountVersion parses the RPC version and makes sure it can actually
// be used.
func (s *rpcServer) determineAccountVersion(
	newVersion poolrpc.AccountVersion) (account.Version, error) { //nolint

	// We can only use the new version of the MuSig2 protocol if we have a
	// recent lnd version that added support for specifying the MuSig2
	// version in the RPC.
	var (
		isMuSig2V100RC2Compatible = false
		currentVersion            = s.server.lndServices.Version
	)
	verErr := lndclient.AssertVersionCompatible(
		currentVersion, muSig2V100RC2Version,
	)
	if verErr == nil {
		isMuSig2V100RC2Compatible = true
	}

	// Now we can do the account version validation.
	switch newVersion {
	case poolrpc.AccountVersion_ACCOUNT_VERSION_LND_DEPENDENT:
		if isMuSig2V100RC2Compatible {
			return account.VersionMuSig2V100RC2, nil
		}

		return account.VersionTaprootEnabled, nil

	case poolrpc.AccountVersion_ACCOUNT_VERSION_LEGACY:
		return account.VersionInitialNoVersion, nil

	case poolrpc.AccountVersion_ACCOUNT_VERSION_TAPROOT:
		return account.VersionTaprootEnabled, nil

	case poolrpc.AccountVersion_ACCOUNT_VERSION_TAPROOT_V2:
		if !isMuSig2V100RC2Compatible {
			return 0, fmt.Errorf("cannot use Taproot v2 enabled "+
				"account version, Pool is connected to lnd "+
				"node version %v but requires at least %v",
				currentVersion.AppMinor,
				muSig2V100RC2Version.AppMinor)
		}

		return account.VersionMuSig2V100RC2, nil

	default:
		return 0, fmt.Errorf("unknown account version %v", newVersion)
	}
}

// AccountModificationFees returns a map from account key to an ordered list of
// account modifying action fees.
func (s *rpcServer) AccountModificationFees(ctx context.Context,
	req *poolrpc.AccountModificationFeesRequest) (
	*poolrpc.AccountModificationFeesResponse, error) {

	// Retrieve all lightning wallet transactions.
	transactionDetails, err := s.lndClient.GetTransactions(
		ctx, &lnrpc.GetTransactionsRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("error retrieving transactions: %v", err)
	}

	// A map from account key to an ordered list of account action fees.
	accountActions := make(map[string][]*poolrpc.AccountModificationFee)

	for _, tx := range transactionDetails.Transactions {
		// Skip transactions which are not related to pool.
		if !account.IsPoolTx(tx) {
			continue
		}

		// Parse account data from transaction label.
		labelData, err := account.ParseTxLabel(tx.Label)
		if err != nil {
			return nil, fmt.Errorf("failed to parse transaction "+
				"label: %v (error: %v)", tx.Label, err)
		}

		// Ignore transaction if account modification not found.
		if labelData == nil {
			continue
		}

		// Select the account specific transaction output. Each account
		// action transaction should include an account specific output.
		output := tx.OutputDetails[labelData.Account.OutputIndex]

		// Construct account modification fee structure.
		acctModFee := &poolrpc.AccountModificationFee{
			Action:       string(labelData.Account.Action),
			Txid:         tx.TxHash,
			BlockHeight:  tx.BlockHeight,
			Timestamp:    tx.TimeStamp,
			OutputAmount: output.Amount,
		}
		acctModFee.Fee = &poolrpc.AccountModificationFee_FeeNull{
			FeeNull: true,
		}
		if labelData.Account.TxFee != nil {
			acctModFee.Fee = &poolrpc.AccountModificationFee_FeeValue{
				FeeValue: int64(*labelData.Account.TxFee),
			}
		}

		// Handle all non-create account actions.
		accountActions[labelData.Account.Key] = append(
			accountActions[labelData.Account.Key], acctModFee,
		)
	}

	result := make(map[string]*poolrpc.ListOfAccountModificationFees)
	for traderKey, modificationFees := range accountActions {
		result[traderKey] = &poolrpc.ListOfAccountModificationFees{
			ModificationFees: modificationFees,
		}
	}

	return &poolrpc.AccountModificationFeesResponse{
		Accounts: result,
	}, nil
}

// rpcOrderStateToDBState maps the order state as received over the RPC
// protocol to the local state that we use in the database.
func rpcOrderStateToDBState(state auctioneerrpc.OrderState) (order.State,
	error) {

	switch state {
	case auctioneerrpc.OrderState_ORDER_SUBMITTED:
		return order.StateSubmitted, nil

	case auctioneerrpc.OrderState_ORDER_CLEARED:
		return order.StateCleared, nil

	case auctioneerrpc.OrderState_ORDER_PARTIALLY_FILLED:
		return order.StatePartiallyFilled, nil

	case auctioneerrpc.OrderState_ORDER_EXECUTED:
		return order.StateExecuted, nil

	case auctioneerrpc.OrderState_ORDER_CANCELED:
		return order.StateCanceled, nil

	case auctioneerrpc.OrderState_ORDER_EXPIRED:
		return order.StateExpired, nil

	case auctioneerrpc.OrderState_ORDER_FAILED:
		return order.StateFailed, nil

	default:
		return 0, fmt.Errorf("unknown state: %v", state)
	}
}

// DBOrderStateToRPCState maps the order state as stored in the database to the
// corresponding RPC enum type.
func DBOrderStateToRPCState(state order.State) (auctioneerrpc.OrderState,
	error) {

	switch state {
	case order.StateSubmitted:
		return auctioneerrpc.OrderState_ORDER_SUBMITTED, nil

	case order.StateCleared:
		return auctioneerrpc.OrderState_ORDER_CLEARED, nil

	case order.StatePartiallyFilled:
		return auctioneerrpc.OrderState_ORDER_PARTIALLY_FILLED, nil

	case order.StateExecuted:
		return auctioneerrpc.OrderState_ORDER_EXECUTED, nil

	case order.StateCanceled:
		return auctioneerrpc.OrderState_ORDER_CANCELED, nil

	case order.StateExpired:
		return auctioneerrpc.OrderState_ORDER_EXPIRED, nil

	case order.StateFailed:
		return auctioneerrpc.OrderState_ORDER_FAILED, nil

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

// unmarshallNodeTier maps the RPC node tier enum to the node tier used in
// memory.
func unmarshallNodeTier(nodeTier auctioneerrpc.NodeTier) (order.NodeTier,
	error) {

	switch nodeTier {
	case auctioneerrpc.NodeTier_TIER_DEFAULT:
		return order.DefaultMinNodeTier, nil

	case auctioneerrpc.NodeTier_TIER_0:
		return order.NodeTier0, nil

	case auctioneerrpc.NodeTier_TIER_1:
		return order.NodeTier1, nil

	default:
		return 0, fmt.Errorf("unknown node tier: %v", nodeTier)
	}
}

// marshallChannelType maps the channel type into the RPC counterpart.
func marshallChannelType(
	channelType order.ChannelType) auctioneerrpc.OrderChannelType {

	switch channelType {
	case order.ChannelTypePeerDependent:
		return auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_PEER_DEPENDENT
	case order.ChannelTypeScriptEnforced:
		return auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_SCRIPT_ENFORCED
	default:
		return auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_UNKNOWN
	}
}

// marshallChannelInfo turns the given channel information map into its RPC
// counterpart.
func marshallChannelInfo(chanInfos map[wire.OutPoint]*chaninfo.ChannelInfo) (
	map[string]*auctioneerrpc.ChannelInfo, error) {

	rpcChannelInfos := make(
		map[string]*auctioneerrpc.ChannelInfo, len(chanInfos),
	)
	for chanPoint, chanInfo := range chanInfos {
		var channelType auctioneerrpc.ChannelType
		switch chanInfo.Version {
		case chanbackup.TweaklessCommitVersion:
			channelType = auctioneerrpc.ChannelType_TWEAKLESS

		// The AnchorsCommitVersion was never widely deployed (at least
		// in mainnet) because the lnd version that included it guarded
		// the anchor channels behind a config flag. Also, the two
		// anchor versions only differ in the fee negotiation and not
		// the commitment TX format, so we don't need to distinguish
		// between them for our purpose.
		case chanbackup.AnchorsCommitVersion,
			chanbackup.AnchorsZeroFeeHtlcTxCommitVersion:
			channelType = auctioneerrpc.ChannelType_ANCHORS

		case chanbackup.ScriptEnforcedLeaseVersion:
			channelType = auctioneerrpc.ChannelType_SCRIPT_ENFORCED_LEASE

		default:
			return nil, fmt.Errorf("unknown channel type: %v",
				chanInfo.Version)
		}
		rpcChannelInfos[chanPoint.String()] = &auctioneerrpc.ChannelInfo{
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

	return rpcChannelInfos, nil
}

func unmarshallSidecar(version order.Version,
	encodedTicket string) (*sidecar.Ticket, error) {

	// An empty ticket means no ticket at all.
	if len(encodedTicket) == 0 {
		return nil, nil
	}

	// Versions previous to the sidecar order version aren't allowed to set
	// the bool value.
	if len(encodedTicket) > 0 && version < order.VersionSidecarChannel {
		return nil, fmt.Errorf("cannot set sidecar for old order " +
			"version")
	}

	// Try to deserialize the ticket.
	return sidecar.DecodeString(encodedTicket)
}

// marshallTicket converts a sidecar ticket into its decoded RPC counterpart.
func marshallTicket(t *sidecar.Ticket) *poolrpc.DecodedSidecarTicket {
	serializePubKey := func(key *btcec.PublicKey) []byte {
		if key == nil {
			return nil
		}

		return key.SerializeCompressed()
	}

	encoded, _ := sidecar.EncodeToString(t)
	resp := &poolrpc.DecodedSidecarTicket{
		Id:                       t.ID[:],
		Version:                  uint32(t.Version),
		State:                    t.State.String(),
		OfferCapacity:            uint64(t.Offer.Capacity),
		OfferPushAmount:          uint64(t.Offer.PushAmt),
		OfferLeaseDurationBlocks: t.Offer.LeaseDurationBlocks,
		OfferSignPubkey:          serializePubKey(t.Offer.SignPubKey),
		OfferAuto:                t.Offer.Auto,
		EncodedTicket:            encoded,
		OfferUnannouncedChannel:  t.Offer.UnannouncedChannel,
		OfferZeroConfChannel:     t.Offer.ZeroConfChannel,
	}

	if t.Offer.SigOfferDigest != nil {
		resp.OfferSignature = t.Offer.SigOfferDigest.Serialize()
	}

	if t.Recipient != nil {
		resp.RecipientMultisigPubkeyIndex = t.Recipient.MultiSigKeyIndex

		resp.RecipientNodePubkey = serializePubKey(
			t.Recipient.NodePubKey,
		)
		resp.RecipientMultisigPubkey = serializePubKey(
			t.Recipient.MultiSigPubKey,
		)
	}

	if t.Order != nil {
		resp.OrderBidNonce = t.Order.BidNonce[:]

		if t.Order.SigOrderDigest != nil {
			resp.OrderSignature = t.Order.SigOrderDigest.Serialize()
		}
	}

	if t.Execution != nil {
		resp.ExecutionPendingChannelId = t.Execution.PendingChannelID[:]
	}

	return resp
}
