package trader

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightninglabs/agora/client/auctioneer"
	"github.com/lightninglabs/agora/client/clientdb"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/loop/lndclient"
)

const (
	// getInfoTimeout is the maximum time we allow for the initial getInfo
	// call to the connected lnd node.
	getInfoTimeout = 30 * time.Second
)

type Server struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	lndServices *lndclient.LndServices
	auctioneer  *auctioneer.Client
	db          *clientdb.DB

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
		quit:        make(chan struct{}),
	}, nil
}

// Start starts the Server, making it ready to accept incoming requests.
func (s *Server) Start() error {
	if !atomic.CompareAndSwapUint32(&s.started, 0, 1) {
		return nil
	}

	log.Infof("Starting trader server")
	s.wg.Add(1)

	// Log connected node.
	ctx, cancel := context.WithTimeout(context.Background(), getInfoTimeout)
	defer cancel()
	info, err := s.lndServices.Client.GetInfo(ctx)
	if err != nil {
		return fmt.Errorf("GetInfo error: %v", err)
	}
	log.Infof("Connected to lnd node %v with pubkey %v",
		info.Alias, hex.EncodeToString(info.IdentityPubkey[:]),
	)

	go s.serverHandler()

	return nil
}

// Stop stops the server.
func (s *Server) Stop() error {
	if !atomic.CompareAndSwapUint32(&s.stopped, 0, 1) {
		return nil
	}

	log.Info("Trader server terminating")
	close(s.quit)
	s.wg.Wait()

	log.Info("Trader server terminated")
	return nil
}

// serverHandler is the main event loop of the server.
func (s *Server) serverHandler() {
	defer s.wg.Done()

	for { // nolint
		// TODO(guggero): Implement main event loop.
		select {

		// In case the server is shutting down.
		case <-s.quit:
			return
		}
	}
}

func (s *Server) InitAccount(ctx context.Context,
	req *clmrpc.InitAccountRequest) (*clmrpc.InitAccountResponse, error) {

	return nil, fmt.Errorf("unimplemented")
}

func (s *Server) ListAccounts(ctx context.Context,
	req *clmrpc.ListAccountsRequest) (*clmrpc.ListAccountsResponse, error) {

	return nil, fmt.Errorf("unimplemented")
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
