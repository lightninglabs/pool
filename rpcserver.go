package client

import (
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
	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/auctioneer"
	"github.com/lightninglabs/agora/client/clientdb"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/agora/client/order"
	"github.com/lightninglabs/loop/lndclient"
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
	auctioneer     *auctioneer.Client
	accountManager *account.Manager
	orderManager   *order.Manager

	quit            chan struct{}
	wg              sync.WaitGroup
	blockNtfnCancel func()
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
	return &rpcServer{
		server:      server,
		lndServices: lnd,
		auctioneer:  server.AuctioneerClient,
		accountManager: account.NewManager(&account.ManagerConfig{
			Store:         accountStore,
			Auctioneer:    server.AuctioneerClient,
			Wallet:        lnd.WalletKit,
			Signer:        lnd.Signer,
			ChainNotifier: lnd.ChainNotifier,
			TxSource:      lnd.Client,
		}),
		orderManager: order.NewManager(&order.ManagerConfig{
			Store:     server.db,
			AcctStore: accountStore,
			Lightning: lnd.Client,
			Wallet:    lnd.WalletKit,
			Signer:    lnd.Signer,
		}),
		quit: make(chan struct{}),
	}
}

// Start starts the rpcServer, making it ready to accept incoming requests.
func (s *rpcServer) Start() error {
	if !atomic.CompareAndSwapUint32(&s.started, 0, 1) {
		return nil
	}

	log.Infof("Starting trader server")

	ctx := context.Background()

	lndCtx, lndCancel := context.WithTimeout(ctx, getInfoTimeout)
	defer lndCancel()
	info, err := s.lndServices.Client.GetInfo(lndCtx)
	if err != nil {
		return fmt.Errorf("error in GetInfo: %v", err)
	}

	log.Infof("Connected to lnd node %v with pubkey %v", info.Alias,
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

	s.wg.Add(1)
	go s.serverHandler(blockChan, blockErrChan)

	log.Infof("Trader server is now active")

	return nil
}

// Stop stops the server.
func (s *rpcServer) Stop() error {
	if !atomic.CompareAndSwapUint32(&s.stopped, 0, 1) {
		return nil
	}

	log.Info("Trader server stopping")
	s.accountManager.Stop()
	s.orderManager.Stop()
	if err := s.auctioneer.Stop(); err != nil {
		log.Errorf("Error closing server stream: %v")
	}

	close(s.quit)
	s.wg.Wait()
	s.blockNtfnCancel()

	log.Info("Stopped trader server")
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

			log.Debugf("Received message from the server: %v", msg)
			err := s.handleServerMessage(msg)
			if err != nil {
				log.Errorf("Error handling server message: %v",
					err)
				err := s.server.Stop()
				if err != nil {
					log.Errorf("Error shutting down: %v",
						err)
				}
			}

		case err := <-s.auctioneer.StreamErrChan:
			// If the server is shutting down, then the client has
			// already scheduled a restart. We only need to handle
			// other errors here.
			if err != nil && err != auctioneer.ErrServerShutdown {
				log.Errorf("Error in server stream: %v", err)
				err := s.auctioneer.HandleServerShutdown(err)
				if err != nil {
					log.Errorf("Error closing stream: %v",
						err)
				}
			}

		case height := <-blockChan:
			log.Infof("Received new block notification: height=%v",
				height)
			s.updateHeight(height)

		case err := <-blockErrChan:
			if err != nil {
				log.Errorf("Unable to receive block "+
					"notification: %v", err)
				err := s.server.Stop()
				if err != nil {
					log.Errorf("Error shutting down: %v",
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
		log.Tracef("Received prepare msg from server, batch_id=%x: %v",
			msg.Prepare.BatchId, spew.Sdump(msg))
		batch, err := order.ParseRPCBatch(msg.Prepare)
		if err != nil {
			return fmt.Errorf("error parsing RPC batch: %v", err)
		}

		// Do an in-depth verification of the batch.
		err = s.orderManager.OrderMatchValidate(batch)
		if err != nil {
			// We can't accept the batch, something went wrong.
			log.Errorf("Error validating batch: %v", err)
			return s.sendRejectBatch(batch, err)
		}

		// Accept the match now.
		//
		// TODO(guggero): Give user the option to bail out of any order
		// up to this point?
		err = s.sendAcceptBatch(batch)
		if err != nil {
			log.Errorf("Error sending accept msg: %v", err)
			return s.sendRejectBatch(batch, err)
		}

		// We were able to accept the batch. Inform the auctioneer, then
		// start negotiating with the remote peers. We'll sign once all
		// channel partners have responded.
		err = s.server.BatchChannelSetup(batch)
		if err != nil {
			log.Errorf("Error setting up channels: %v", err)
			return s.sendRejectBatch(batch, err)
		}

		// Sign for the accounts in the batch.
		sigs, err := s.orderManager.BatchSign()
		if err != nil {
			log.Errorf("Error signing batch: %v", err)
			return s.sendRejectBatch(batch, err)
		}
		err = s.sendSignBatch(batch, sigs)
		if err != nil {
			log.Errorf("Error sending sign msg: %v", err)
			return s.sendRejectBatch(batch, err)
		}

	// The previously prepared batch has been executed and we can finalize
	// it by opening the channel and persisting the account and order diffs.
	case *clmrpc.ServerAuctionMessage_Finalize:
		log.Tracef("Received finalize msg from server, batch_id=%x: %v",
			msg.Finalize.BatchId, spew.Sdump(msg))

		var batchID order.BatchID
		copy(batchID[:], msg.Finalize.BatchId)
		return s.orderManager.BatchFinalize(batchID)

	default:
		return fmt.Errorf("unknown server message: %v", msg)
	}

	return nil
}

func (s *rpcServer) InitAccount(ctx context.Context,
	req *clmrpc.InitAccountRequest) (*clmrpc.Account, error) {

	account, err := s.accountManager.InitAccount(
		ctx, btcutil.Amount(req.AccountValue), req.AccountExpiry,
		atomic.LoadUint32(&s.bestHeight),
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

	default:
		return nil, fmt.Errorf("unknown state %v", a.State)
	}

	var closeTxHash chainhash.Hash
	if a.CloseTx != nil {
		closeTxHash = a.CloseTx.TxHash()
	}

	return &clmrpc.Account{
		TraderKey: a.TraderKey.PubKey.SerializeCompressed(),
		Outpoint: &clmrpc.OutPoint{
			Txid:        a.OutPoint.Hash[:],
			OutputIndex: a.OutPoint.Index,
		},
		Value:            uint32(a.Value),
		ExpirationHeight: a.Expiry,
		State:            rpcState,
		CloseTxid:        closeTxHash[:],
	}, nil
}

func (s *rpcServer) CloseAccount(ctx context.Context,
	req *clmrpc.CloseAccountRequest) (*clmrpc.CloseAccountResponse, error) {

	traderKey, err := btcec.ParsePubKey(req.TraderKey, btcec.S256())
	if err != nil {
		return nil, err
	}

	var closeOutputs []*wire.TxOut
	if len(req.Outputs) > 0 {
		closeOutputs = make([]*wire.TxOut, 0, len(req.Outputs))
		for _, output := range req.Outputs {
			// Make sure they've provided a valid output script.
			_, err := txscript.ParsePkScript(output.Script)
			if err != nil {
				return nil, err
			}

			closeOutputs = append(closeOutputs, &wire.TxOut{
				Value:    int64(output.Value),
				PkScript: output.Script,
			})
		}
	}

	closeTx, err := s.accountManager.CloseAccount(
		ctx, traderKey, closeOutputs, atomic.LoadUint32(&s.bestHeight),
	)
	if err != nil {
		return nil, err
	}
	closeTxHash := closeTx.TxHash()

	return &clmrpc.CloseAccountResponse{
		CloseTxid: closeTxHash[:],
	}, nil
}

func (s *rpcServer) ModifyAccount(ctx context.Context,
	req *clmrpc.ModifyAccountRequest) (
	*clmrpc.ModifyAccountResponse, error) {

	return nil, fmt.Errorf("unimplemented")
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
			MaxDuration: uint32(a.MaxDurationBlocks),
		}

	case *clmrpc.SubmitOrderRequest_Bid:
		b := requestOrder.Bid
		kit, err := order.ParseRPCOrder(b.Version, b.Details)
		if err != nil {
			return nil, err
		}
		o = &order.Bid{
			Kit:         *kit,
			MinDuration: uint32(b.MinDurationBlocks),
		}

	default:
		return nil, fmt.Errorf("invalid order request")
	}

	// Verify that the account exists.
	acct, err := s.server.db.Account(o.Details().AcctKey)
	if err != nil {
		return nil, fmt.Errorf("cannot accept order: %v", err)
	}

	// Collect all the order data and sign it before sending it to the
	// auction server.
	serverParams, err := s.orderManager.PrepareOrder(ctx, o, acct)
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
			log.Errorf("Could not delete failed order: %v", err2)
		}

		// If there was something wrong with the information the user
		// provided, then return this as a nice string instead of an
		// error type.
		if userErr, ok := err.(*order.UserError); ok {
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

	// ServerOrder is accepted.
	return &clmrpc.SubmitOrderResponse{
		Details: &clmrpc.SubmitOrderResponse_Accepted{
			Accepted: true,
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

	// The RPC is split by order type so we have to separate them now.
	asks := make([]*clmrpc.Ask, 0, len(dbOrders))
	bids := make([]*clmrpc.Bid, 0, len(dbOrders))
	for _, dbOrder := range dbOrders {
		nonce := dbOrder.Nonce()

		// Ask the server about the order's current status.
		state, unitsUnfullfilled, err := s.auctioneer.OrderState(
			ctx, nonce,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to query order state on"+
				"server for order %v: %v", nonce.String(), err)
		}

		dbDetails := dbOrder.Details()
		details := &clmrpc.Order{
			UserSubKey:       dbDetails.AcctKey.SerializeCompressed(),
			RateFixed:        int64(dbDetails.FixedRate),
			Amt:              int64(dbDetails.Amt),
			FundingFeeRate:   int64(dbDetails.FixedRate),
			OrderNonce:       nonce[:],
			State:            state.String(),
			Units:            uint32(dbDetails.Units),
			UnitsUnfulfilled: unitsUnfullfilled,
		}

		switch o := dbOrder.(type) {
		case *order.Ask:
			rpcAsk := &clmrpc.Ask{
				Details:           details,
				MaxDurationBlocks: int64(o.MaxDuration),
				Version:           uint32(o.Version),
			}
			asks = append(asks, rpcAsk)

		case *order.Bid:
			rpcBid := &clmrpc.Bid{
				Details:           details,
				MinDurationBlocks: int64(o.MinDuration),
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
	err := s.auctioneer.CancelOrder(ctx, nonce)
	if err != nil {
		return nil, err
	}
	return &clmrpc.CancelOrderResponse{}, nil
}

// sendRejectBatch sends a reject message to the server with the properly
// decoded reason code and the full reason message as a string.
func (s *rpcServer) sendRejectBatch(batch *order.Batch, failure error) error {
	msg := &clmrpc.ClientAuctionMessage_Reject{
		Reject: &clmrpc.OrderMatchReject{
			BatchId: batch.ID[:],
			Reason:  failure.Error(),
		},
	}

	// Attach the status code to the message to give a bit more context.
	switch {
	case errors.Is(failure, order.ErrVersionMismatch):
		msg.Reject.ReasonCode = clmrpc.OrderMatchReject_BATCH_VERSION_MISMATCH

	case errors.Is(failure, order.ErrMismatchErr):
		msg.Reject.ReasonCode = clmrpc.OrderMatchReject_SERVER_MISBEHAVIOR

	default:
		msg.Reject.ReasonCode = clmrpc.OrderMatchReject_UNKNOWN
	}
	log.Infof("Sending batch rejection message for batch %x with "+
		"code %v and message: %v", batch.ID, msg.Reject.ReasonCode,
		failure)

	// Send the message to the server. If a new error happens we return that
	// one because we know the causing error has at least been logged at
	// some point before.
	err := s.auctioneer.SendAuctionMessage(&clmrpc.ClientAuctionMessage{
		Msg: msg,
	})
	if err != nil {
		return fmt.Errorf("error sending reject message: %v", err)
	}
	return failure
}

// sendAcceptBatch sends an accept message to the server with the list of order
// nonces that we accept in the batch.
func (s *rpcServer) sendAcceptBatch(batch *order.Batch) error {
	// Prepare the list of nonces we accept by serializing them to a slice
	// of byte slices.
	nonces := make([][]byte, 0, len(batch.MatchedOrders))
	idx := 0
	for nonce := range batch.MatchedOrders {
		nonces[idx] = nonce[:]
		idx++
	}

	// Send the message to the server.
	return s.auctioneer.SendAuctionMessage(&clmrpc.ClientAuctionMessage{
		Msg: &clmrpc.ClientAuctionMessage_Accept{
			Accept: &clmrpc.OrderMatchAccept{
				BatchId:    batch.ID[:],
				OrderNonce: nonces,
			},
		},
	})
}

// sendSignBatch sends a sign message to the server with the witness stacks of
// all accounts that are involved in the batch.
func (s *rpcServer) sendSignBatch(batch *order.Batch,
	sigs order.BatchSignature) error {

	// Prepare the list of witness stack messages and send them to the
	// server.
	rpcSigs := make(map[string][]byte)
	for acctKey, sig := range sigs {
		key := hex.EncodeToString(acctKey[:])
		rpcSigs[key] = sig.Serialize()
	}
	return s.auctioneer.SendAuctionMessage(&clmrpc.ClientAuctionMessage{
		Msg: &clmrpc.ClientAuctionMessage_Sign{
			Sign: &clmrpc.OrderMatchSign{
				BatchId:     batch.ID[:],
				AccountSigs: rpcSigs,
			},
		},
	})
}
