package trader

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/auctioneer"
	"github.com/lightninglabs/agora/client/clientdb"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/loop/lndclient"
)

const (
	// getInfoTimeout is the maximum time we allow for the initial getInfo
	// call to the connected lnd node.
	getInfoTimeout = 5 * time.Second
)

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

	quit chan struct{}
	wg   sync.WaitGroup
}

func NewServer(lnd *lndclient.LndServices, auctionServer *auctioneer.Client,
	dbDir string) (*Server, error) {

	db, err := clientdb.New(dbDir)
	if err != nil {
		return nil, err
	}

	return &Server{
		lndServices: lnd,
		auctioneer:  auctionServer,
		db:          db,
		accountManager: account.NewManager(&account.ManagerConfig{
			Store:         db,
			Auctioneer:    auctionServer,
			Wallet:        lnd.WalletKit,
			ChainNotifier: lnd.ChainNotifier,
			TxSource:      lnd.Client,
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
		return fmt.Errorf("unable to call GetInfo on lnd node: %v", err)
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

	if err := s.accountManager.Start(); err != nil {
		return err
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

	log.Info("Stopping trader server")

	s.accountManager.Stop()

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
	req *clmrpc.InitAccountRequest) (*clmrpc.InitAccountResponse, error) {

	account, err := s.accountManager.InitAccount(
		ctx, btcutil.Amount(req.AccountValue), req.AccountExpiry,
		atomic.LoadUint32(&s.bestHeight),
	)
	if err != nil {
		return nil, err
	}

	return &clmrpc.InitAccountResponse{
		AccountPoint: &clmrpc.OutPoint{
			Txid:        account.OutPoint.Hash[:],
			OutputIndex: account.OutPoint.Index,
		},
	}, nil
}

func (s *Server) ListAccounts(ctx context.Context,
	req *clmrpc.ListAccountsRequest) (*clmrpc.ListAccountsResponse, error) {

	accounts, err := s.db.Accounts()
	if err != nil {
		return nil, err
	}

	rpcAccounts := make([]*clmrpc.Account, 0, len(accounts))
	for _, a := range accounts {
		var rpcState clmrpc.AccountState
		switch a.State {
		case account.StateInitiated, account.StatePendingOpen:
			rpcState = clmrpc.AccountState_PENDING_OPEN

		case account.StateOpen:
			rpcState = clmrpc.AccountState_OPEN

		default:
			return nil, fmt.Errorf("unknown state %v", a.State)
		}

		rpcAccounts = append(rpcAccounts, &clmrpc.Account{
			TraderKey: a.TraderKey[:],
			Outpoint: &clmrpc.OutPoint{
				Txid:        a.OutPoint.Hash[:],
				OutputIndex: a.OutPoint.Index,
			},
			Value:            uint32(a.Value),
			ExpirationHeight: a.Expiry,
			State:            rpcState,
		})
	}

	return &clmrpc.ListAccountsResponse{
		Accounts: rpcAccounts,
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

func (s *Server) SubmitOrder(ctx context.Context,
	req *clmrpc.SubmitOrderRequest) (*clmrpc.SubmitOrderResponse, error) {

	return nil, fmt.Errorf("unimplemented")
}

func (s *Server) ListOrders(ctx context.Context,
	req *clmrpc.ListOrdersRequest) (*clmrpc.ListOrdersResponse, error) {

	return nil, fmt.Errorf("unimplemented")
}

func (s *Server) CancelOrder(ctx context.Context,
	req *clmrpc.CancelOrderRequest) (*clmrpc.CancelOrderResponse, error) {

	return nil, fmt.Errorf("unimplemented")
}
