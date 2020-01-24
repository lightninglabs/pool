package trader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/auctioneer"
	"github.com/lightninglabs/agora/client/clientdb"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/agora/client/order"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// getInfoTimeout is the maximum time we allow for the initial getInfo
	// call to the connected lnd node.
	getInfoTimeout = 5 * time.Second
)

// Server implements the gRPC server on the client side and answers RPC calls
// from an end user client program like the command line interface.
type Server struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	// bestHeight is the best known height of the main chain. This MUST be
	// used atomically.
	bestHeight uint32

	lndServices    *lndclient.LndServices
	auctioneer     *auctioneer.Client
	db             *clientdb.DB
	accountManager *account.Manager
	orderManager   *order.Manager

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewServer creates a new client-side RPC server that uses the given connection
// to the trader's lnd node and the auction server. A client side database is
// created in `serverDir` if it does not yet exist.
func NewServer(lnd *lndclient.LndServices, auctioneer *auctioneer.Client,
	serverDir string) (*Server, error) {

	db, err := clientdb.New(serverDir)
	if err != nil {
		return nil, err
	}

	return &Server{
		lndServices: lnd,
		auctioneer:  auctioneer,
		db:          db,
		accountManager: account.NewManager(&account.ManagerConfig{
			Store:         db,
			Auctioneer:    auctioneer,
			Wallet:        lnd.WalletKit,
			Signer:        lnd.Signer,
			ChainNotifier: lnd.ChainNotifier,
			TxSource:      lnd.Client,
		}),
		orderManager: order.NewManager(&order.ManagerConfig{
			Store:     db,
			Lightning: lnd.Client,
			Wallet:    lnd.WalletKit,
			Signer:    lnd.Signer,
		}),
		quit: make(chan struct{}),
	}, nil
}

// Start starts the Server, making it ready to accept incoming requests.
func (s *Server) Start() error {
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

	chainNotifier := s.lndServices.ChainNotifier
	blockChan, blockErrChan, err := chainNotifier.RegisterBlockEpochNtfn(ctx)
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
func (s *Server) Stop() error {
	if !atomic.CompareAndSwapUint32(&s.stopped, 0, 1) {
		return nil
	}

	log.Info("Trader server stopping")
	s.accountManager.Stop()
	s.orderManager.Stop()
	_ = s.db.Close()

	close(s.quit)
	s.wg.Wait()

	log.Info("Stopped trader server")
	return nil
}

// serverHandler is the main event loop of the server.
func (s *Server) serverHandler(blockChan chan int32, blockErrChan chan error) {
	defer s.wg.Done()

	for {
		select {
		case height := <-blockChan:
			log.Infof("Received new block notification: height=%v",
				height)
			s.updateHeight(height)

		case err := <-blockErrChan:
			if err != nil {
				log.Errorf("Unable to receive block "+
					"notification: %v", err)
			}

		// In case the server is shutting down.
		case <-s.quit:
			return
		}
	}
}

func (s *Server) updateHeight(height int32) {
	// Store height atomically so the incoming request handler can access it
	// without locking.
	atomic.StoreUint32(&s.bestHeight, uint32(height))
}

func (s *Server) InitAccount(ctx context.Context,
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

func (s *Server) ListAccounts(ctx context.Context,
	req *clmrpc.ListAccountsRequest) (*clmrpc.ListAccountsResponse, error) {

	accounts, err := s.db.Accounts()
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

	case account.StateOpen:
		rpcState = clmrpc.AccountState_OPEN

	default:
		return nil, fmt.Errorf("unknown state %v", a.State)
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
	}, nil
}

func (s *Server) CloseAccount(ctx context.Context,
	req *clmrpc.CloseAccountRequest) (*clmrpc.CloseAccountResponse, error) {

	return nil, fmt.Errorf("unimplemented")
}

func (s *Server) ModifyAccount(ctx context.Context,
	req *clmrpc.ModifyAccountRequest) (
	*clmrpc.ModifyAccountResponse, error) {

	return nil, fmt.Errorf("unimplemented")
}

// SubmitOrder assembles all the information that is required to submit an order
// from the trader's lnd node, signs it and then sends the order to the server
// to be included in the auctioneer's order book.
func (s *Server) SubmitOrder(ctx context.Context,
	req *clmrpc.SubmitOrderRequest) (*clmrpc.SubmitOrderResponse, error) {

	var o order.Order
	switch requestOrder := req.Details.(type) {
	case *clmrpc.SubmitOrderRequest_Ask:
		a := requestOrder.Ask
		kit, err := parseRPCOrder(a.Version, a.Details)
		if err != nil {
			return nil, err
		}
		o = &order.Ask{
			Kit:         *kit,
			MaxDuration: uint32(a.MaxDurationBlocks),
		}

	case *clmrpc.SubmitOrderRequest_Bid:
		b := requestOrder.Bid
		kit, err := parseRPCOrder(b.Version, b.Details)
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
	acct, err := s.db.Account(o.Details().AcctKey)
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
		if err2 := s.db.DelOrder(o.Nonce()); err2 != nil {
			log.Errorf("could not delete failed order: %v", err2)
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
func (s *Server) ListOrders(ctx context.Context, _ *clmrpc.ListOrdersRequest) (
	*clmrpc.ListOrdersResponse, error) {

	// Get all orders from our local store first.
	dbOrders, err := s.db.GetOrders()
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
func (s *Server) CancelOrder(ctx context.Context,
	req *clmrpc.CancelOrderRequest) (*clmrpc.CancelOrderResponse, error) {

	var nonce order.Nonce
	copy(nonce[:], req.OrderNonce)
	err := s.auctioneer.CancelOrder(ctx, nonce)
	if err != nil {
		return nil, err
	}
	return &clmrpc.CancelOrderResponse{}, nil
}

// parseRPCOrder parses the incoming raw RPC order into the go native data
// types used in the order struct.
func parseRPCOrder(version uint32, details *clmrpc.Order) (*order.Kit, error) {
	var nonce order.Nonce
	copy(nonce[:], details.OrderNonce)
	kit := order.NewKit(nonce)

	// If the user didn't provide a nonce, we generate one.
	if nonce == order.ZeroNonce {
		preimageBytes, err := randomPreimage()
		if err != nil {
			return nil, fmt.Errorf("cannot generate nonce: %v", err)
		}
		var preimage lntypes.Preimage
		copy(preimage[:], preimageBytes)
		kit = order.NewKitWithPreimage(preimage)
	}

	pubKey, err := btcec.ParsePubKey(details.UserSubKey, btcec.S256())
	if err != nil {
		return nil, fmt.Errorf("error parsing account key: %v", err)
	}

	kit.AcctKey = pubKey
	kit.Version = order.Version(version)
	kit.FixedRate = uint32(details.RateFixed)
	kit.Amt = btcutil.Amount(details.Amt)
	kit.FundingFeeRate = chainfee.SatPerKWeight(details.FundingFeeRate)
	kit.Units = order.NewSupplyFromSats(kit.Amt)
	kit.UnitsUnfulfilled = kit.Units
	return kit, nil
}

// randomPreimage creates a new preimage from a random number generator.
func randomPreimage() ([]byte, error) {
	var nonce order.Nonce
	_, err := rand.Read(nonce[:])
	if err != nil {
		return nil, err
	}
	return nonce[:], nil
}
