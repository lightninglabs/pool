package auctioneer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/agora/client/order"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	initialConnectRetries = 3
	reconnectRetries      = 10
)

var (
	// ErrServerShutdown is the error that is returned if the auction server
	// signals it's going to shut down.
	ErrServerShutdown = errors.New("server shutting down")

	// ErrClientShutdown is the error that is returned if the trader client
	// itself is shutting down.
	ErrClientShutdown = errors.New("client shutting down")
)

// Config holds the configuration options for the auctioneer client.
type Config struct {
	// ServerAddress is the domain:port of the auctioneer server.
	ServerAddress string

	// Insecure signals that no TLS should be used if set to true.
	Insecure bool

	// TLSPathServer is the path to a local file that holds the auction
	// server's TLS certificate. This is only needed if the server is using
	// a self signed cert.
	TLSPathServer string

	// DialOpts is a list of additional options that should be used when
	// dialing the gRPC connection.
	DialOpts []grpc.DialOption

	// Signer is the signing interface that is used to sign messages during
	// the authentication handshake with the auctioneer server.
	Signer lndclient.SignerClient

	// MinBackoff is the minimum time that is waited before the next re-
	// connect attempt is made. After each try the backoff is doubled until
	// MaxBackoff is reached.
	MinBackoff time.Duration

	// MaxBackoff is the maximum time that is waited between connection
	// attempts.
	MaxBackoff time.Duration

	// BatchSource provides information about the current pending batch, if
	// any.
	BatchSource BatchSource
}

// Client performs the client side part of auctions. This interface exists to be
// able to implement a stub.
type Client struct {
	cfg *Config

	StreamErrChan  chan error
	FromServerChan chan *clmrpc.ServerAuctionMessage

	serverConn *grpc.ClientConn
	client     clmrpc.ChannelAuctioneerClient

	quit            chan struct{}
	wg              sync.WaitGroup
	serverStream    clmrpc.ChannelAuctioneer_SubscribeBatchAuctionClient
	streamMutex     sync.Mutex
	streamCancel    func()
	subscribedAccts map[[33]byte]*acctSubscription
}

// NewClient returns a new instance to initiate auctions with.
func NewClient(cfg *Config) (*Client, error) {
	var err error
	cfg.DialOpts, err = getAuctionServerDialOpts(
		cfg.Insecure, cfg.TLSPathServer, cfg.DialOpts...,
	)
	if err != nil {
		return nil, err
	}

	return &Client{
		cfg:             cfg,
		FromServerChan:  make(chan *clmrpc.ServerAuctionMessage),
		StreamErrChan:   make(chan error),
		quit:            make(chan struct{}),
		subscribedAccts: make(map[[33]byte]*acctSubscription),
	}, nil
}

// Start starts the client, establishing the connection to the server.
func (c *Client) Start() error {
	serverConn, err := grpc.Dial(c.cfg.ServerAddress, c.cfg.DialOpts...)
	if err != nil {
		return fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	c.serverConn = serverConn
	c.client = clmrpc.NewChannelAuctioneerClient(serverConn)

	return nil
}

// getAuctionServerDialOpts returns the dial options to connect to the auction
// server.
func getAuctionServerDialOpts(insecure bool, tlsPath string,
	dialOpts ...grpc.DialOption) ([]grpc.DialOption, error) {

	// Create a copy of the dial options array.
	opts := dialOpts

	// There are three options to connect to a auction server, either
	// insecure, using a self-signed certificate or with a certificate
	// signed by a public CA.
	switch {
	case insecure:
		opts = append(opts, grpc.WithInsecure())

	case tlsPath != "":
		// Load the specified TLS certificate and build
		// transport credentials
		creds, err := credentials.NewClientTLSFromFile(tlsPath, "")
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))

	default:
		creds := credentials.NewTLS(&tls.Config{})
		opts = append(opts, grpc.WithTransportCredentials(creds))
	}

	return opts, nil
}

// Stop shuts down the client connection to the auction server.
func (c *Client) Stop() error {
	log.Infof("Shutting down auctioneer client")
	close(c.quit)
	err := c.closeStream()
	if err != nil {
		log.Errorf("Unable to close stream: %v", err)
	}
	c.wg.Wait()
	close(c.FromServerChan)
	return c.serverConn.Close()
}

// closeStream closes the long-lived stream connection to the server.
func (c *Client) closeStream() error {
	c.streamMutex.Lock()
	defer c.streamMutex.Unlock()

	if c.serverStream == nil {
		return nil
	}
	log.Debugf("Closing server stream")
	err := c.serverStream.CloseSend()
	c.streamCancel()
	return err
}

// ReserveAccount reserves an account with the auctioneer. It returns the base
// public key we should use for them in our 2-of-2 multi-sig construction, and
// the initial batch key.
func (c *Client) ReserveAccount(ctx context.Context) (*account.Reservation, error) {
	resp, err := c.client.ReserveAccount(ctx, &clmrpc.ReserveAccountRequest{})
	if err != nil {
		return nil, err
	}

	auctioneerKey, err := btcec.ParsePubKey(
		resp.AuctioneerKey, btcec.S256(),
	)
	if err != nil {
		return nil, err
	}
	initialBatchKey, err := btcec.ParsePubKey(
		resp.InitialBatchKey, btcec.S256(),
	)
	if err != nil {
		return nil, err
	}

	return &account.Reservation{
		AuctioneerKey:   auctioneerKey,
		InitialBatchKey: initialBatchKey,
	}, nil
}

// InitAccount initializes an account with the auctioneer such that it can be
// used once fully confirmed.
func (c *Client) InitAccount(ctx context.Context, account *account.Account) error {
	accountOutput, err := account.Output()
	if err != nil {
		return fmt.Errorf("unable to construct account output: %v", err)
	}

	_, err = c.client.InitAccount(ctx, &clmrpc.ServerInitAccountRequest{
		AccountPoint: &clmrpc.OutPoint{
			Txid:        account.OutPoint.Hash[:],
			OutputIndex: account.OutPoint.Index,
		},
		AccountScript: accountOutput.PkScript,
		AccountValue:  uint32(account.Value),
		AccountExpiry: account.Expiry,
		UserSubKey:    account.TraderKey.PubKey.SerializeCompressed(),
	})
	return err
}

// ModifyAccount sends an intent to the auctioneer that we'd like to modify the
// account with the associated trader key. The auctioneer's signature is
// returned, allowing us to broadcast a transaction spending from the account
// allowing our modifications to take place. The inputs and outputs provided
// should exclude the account input being spent and the account output
// potentially being recreated, since the auctioneer can construct those
// themselves. If no modifiers are present, then the auctioneer will interpret
// the request as an account closure.
func (c *Client) ModifyAccount(ctx context.Context, account *account.Account,
	inputs []*wire.TxIn, outputs []*wire.TxOut,
	modifiers []account.Modifier) ([]byte, error) {

	rpcInputs := make([]*clmrpc.ServerInput, 0, len(inputs))
	for _, input := range inputs {
		op := input.PreviousOutPoint
		rpcInputs = append(rpcInputs, &clmrpc.ServerInput{
			Outpoint: &clmrpc.OutPoint{
				Txid:        op.Hash[:],
				OutputIndex: op.Index,
			},
		})
	}

	rpcOutputs := make([]*clmrpc.ServerOutput, 0, len(outputs))
	for _, output := range outputs {
		rpcOutputs = append(rpcOutputs, &clmrpc.ServerOutput{
			Value:  uint32(output.Value),
			Script: output.PkScript,
		})
	}

	var rpcNewParams *clmrpc.ServerModifyAccountRequest_NewAccountParameters
	modifiedAccount := account.Copy(modifiers...)
	if len(modifiers) > 0 {
		rpcNewParams = &clmrpc.ServerModifyAccountRequest_NewAccountParameters{
			Value: uint32(modifiedAccount.Value),
		}
	}

	resp, err := c.client.ModifyAccount(ctx, &clmrpc.ServerModifyAccountRequest{
		UserSubKey: account.TraderKey.PubKey.SerializeCompressed(),
		NewInputs:  rpcInputs,
		NewOutputs: rpcOutputs,
		NewParams:  rpcNewParams,
	})
	if err != nil {
		return nil, err
	}

	return resp.AccountSig, nil
}

// SubmitOrder sends a fully finished order message to the server and interprets
// its answer.
func (c *Client) SubmitOrder(ctx context.Context, o order.Order,
	serverParams *order.ServerOrderParams) error {

	// Prepare everything that is common to both ask and bid orders.
	nonce := o.Nonce()
	rpcRequest := &clmrpc.ServerSubmitOrderRequest{}
	nodeAddrs := make([]*clmrpc.NodeAddress, 0, len(serverParams.Addrs))
	for _, addr := range serverParams.Addrs {
		nodeAddrs = append(nodeAddrs, &clmrpc.NodeAddress{
			Network: addr.Network(),
			Addr:    addr.String(),
		})
	}
	details := &clmrpc.ServerOrder{
		UserSubKey:             o.Details().AcctKey.SerializeCompressed(),
		RateFixed:              int64(o.Details().FixedRate),
		Amt:                    int64(o.Details().Amt),
		OrderNonce:             nonce[:],
		OrderSig:               serverParams.RawSig,
		MultiSigKey:            serverParams.MultiSigKey[:],
		NodePub:                serverParams.NodePubkey[:],
		NodeAddr:               nodeAddrs,
		FundingFeeRateSatPerKw: int64(o.Details().FundingFeeRate),
	}

	// Split into server message which is type specific.
	switch castOrder := o.(type) {
	case *order.Ask:
		serverAsk := &clmrpc.ServerAsk{
			Details:           details,
			MaxDurationBlocks: int64(castOrder.MaxDuration),
			Version:           uint32(castOrder.Version),
		}
		rpcRequest.Details = &clmrpc.ServerSubmitOrderRequest_Ask{
			Ask: serverAsk,
		}

	case *order.Bid:
		serverBid := &clmrpc.ServerBid{
			Details:           details,
			MinDurationBlocks: int64(castOrder.MinDuration),
			Version:           uint32(castOrder.Version),
		}
		rpcRequest.Details = &clmrpc.ServerSubmitOrderRequest_Bid{
			Bid: serverBid,
		}

	default:
		return fmt.Errorf("invalid order type: %v", castOrder)
	}

	// Submit the finished request and parse the response.
	resp, err := c.client.SubmitOrder(ctx, rpcRequest)
	if err != nil {
		return err
	}
	switch submitResp := resp.Details.(type) {
	case *clmrpc.ServerSubmitOrderResponse_InvalidOrder:
		return &order.UserError{
			FailMsg: submitResp.InvalidOrder.FailString,
			Details: submitResp.InvalidOrder,
		}

	case *clmrpc.ServerSubmitOrderResponse_Accepted:
		return nil

	default:
		return fmt.Errorf("unknown server response: %v", resp)
	}
}

// CancelOrder sends an order cancellation message to the server.
func (c *Client) CancelOrder(ctx context.Context, nonce order.Nonce) error {
	_, err := c.client.CancelOrder(ctx, &clmrpc.ServerCancelOrderRequest{
		OrderNonce: nonce[:],
	})
	return err
}

// OrderState queries the state of an order on the server. This only returns the
// state as it's currently known to the server's database. For real-time updates
// on the state, the SubscribeBatchAuction stream should be used.
func (c *Client) OrderState(ctx context.Context, nonce order.Nonce) (
	order.State, uint32, error) {

	resp, err := c.client.OrderState(ctx, &clmrpc.ServerOrderStateRequest{
		OrderNonce: nonce[:],
	})
	if err != nil {
		return 0, 0, err
	}

	// Map RPC state to internal state.
	switch resp.State {
	case clmrpc.OrderState_ORDER_SUBMITTED:
		return order.StateSubmitted, resp.UnitsUnfulfilled, nil

	case clmrpc.OrderState_ORDER_CLEARED:
		return order.StateCleared, resp.UnitsUnfulfilled, nil

	case clmrpc.OrderState_ORDER_PARTIALLY_FILLED:
		return order.StatePartiallyFilled, resp.UnitsUnfulfilled, nil

	case clmrpc.OrderState_ORDER_EXECUTED:
		return order.StateExecuted, resp.UnitsUnfulfilled, nil

	case clmrpc.OrderState_ORDER_CANCELED:
		return order.StateCanceled, resp.UnitsUnfulfilled, nil

	case clmrpc.OrderState_ORDER_EXPIRED:
		return order.StateExpired, resp.UnitsUnfulfilled, nil

	case clmrpc.OrderState_ORDER_FAILED:
		return order.StateFailed, resp.UnitsUnfulfilled, nil

	default:
		return 0, 0, fmt.Errorf("invalid order state: %v", resp.State)
	}
}

// SubscribeAccountUpdates opens a stream to the server and subscribes
// to all updates that concern the given account, including all orders
// that spend from that account. Only a single stream is ever open to
// the server, so a second call to this method will send a second
// subscription over the same stream, multiplexing all messages into the
// same connection. A stream can be long-lived, so this can be called
// for every account as soon as it's confirmed open.
func (c *Client) SubscribeAccountUpdates(ctx context.Context,
	acctKey *keychain.KeyDescriptor) error {

	var acctPubKey [33]byte
	copy(acctPubKey[:], acctKey.PubKey.SerializeCompressed())

	// Don't subscribe more than once.
	if _, ok := c.subscribedAccts[acctPubKey]; ok {
		return nil
	}

	c.streamMutex.Lock()
	defer c.streamMutex.Unlock()

	if c.serverStream == nil {
		err := c.connectServerStream(0, initialConnectRetries)
		if err != nil {
			return err
		}

		// Since this is the first time we establish our connection to
		// the auctioneer, check whether we need to mark our pending
		// batch as finalized, or if we need to remove it due to the
		// batch auction no longer including us.
		if err := c.checkPendingBatch(); err != nil {
			return err
		}
	}

	// Before we can expect to receive any updates, we need to perform the
	// 3-way authentication handshake.
	sub := &acctSubscription{
		acctKey:       acctKey,
		sendMsg:       c.SendAuctionMessage,
		signer:        c.cfg.Signer,
		challengeChan: make(chan [32]byte),
	}
	c.subscribedAccts[acctPubKey] = sub
	return sub.authenticate(ctx)
}

// SendAuctionMessage sends an auction message through the long-lived stream to
// the auction server. A message can only be sent as a response to a server
// message, therefore the stream must already be open.
func (c *Client) SendAuctionMessage(msg *clmrpc.ClientAuctionMessage) error {
	if c.serverStream == nil {
		return fmt.Errorf("cannot send message, stream not open")
	}

	return c.serverStream.Send(msg)
}

// wait blocks for a given amount of time but returns immediately if the client
// is shutting down.
func (c *Client) wait(backoff time.Duration) error {
	if backoff > 0 {
		select {
		case <-time.After(backoff):
		case <-c.quit:
			return ErrClientShutdown
		}
	}
	return nil
}

// connectServerStream opens the initial connection to the server for the stream
// of account updates and handles reconnect trials with incremental backoff.
func (c *Client) connectServerStream(initialBackoff time.Duration,
	numRetries int) error {

	var (
		backoff = initialBackoff
		ctx     context.Context
		err     error
	)
	for i := 0; i < numRetries; i++ {
		// Wait before connecting in case this is a reconnect trial.
		err = c.wait(backoff)
		if err != nil {
			return err
		}
		ctx, c.streamCancel = context.WithCancel(context.Background())
		c.serverStream, err = c.client.SubscribeBatchAuction(ctx)
		if err == nil {
			log.Debugf("Connected successfully to server after "+
				"%d tries", i+1)
			break
		}

		// Connect wasn't successful, cancel the context and increase
		// the time we'll wait until the next try.
		backoff *= 2
		if backoff == 0 {
			backoff = c.cfg.MinBackoff
		}
		if backoff > c.cfg.MaxBackoff {
			backoff = c.cfg.MaxBackoff
		}
		log.Debugf("Connect failed with error, canceling and backing "+
			"off for %s: %v", backoff, err)
		c.streamCancel()
		log.Infof("Connection to server failed, will try again in %v",
			backoff)
	}
	if err != nil {
		log.Errorf("Connection to server failed after %d retries",
			numRetries)
		return err
	}

	// Read incoming messages and send them to the channel where
	// the order manager is listening on. We can only send our first message
	// to the server after we've received the challenge, which we'll track
	// with its own wait group.
	log.Infof("Successfully connected to auction server")
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.readIncomingStream()
	}()

	return nil
}

// readIncomingStream reads incoming messages on a server update stream.
// Messages read from the stream are placed in the FromServerChan channel.
//
// NOTE: This method must be called as a subroutine because it blocks as long as
// the stream is open.
func (c *Client) readIncomingStream() {
	for {
		// Cancel the stream on client shutdown.
		select {
		case <-c.quit:
			return

		default:
		}

		// Read next message from server.
		msg, err := c.serverStream.Recv()
		log.Tracef("Received msg=%s, err=%v from server",
			spew.Sdump(msg), err)
		switch {
		// EOF is the "normal" close signal, meaning the server has
		// cut its side of the connection. We will only get this during
		// the proper shutdown of the server where we already have a
		// reconnect scheduled. On an improper shutdown, we'll get an
		// error, usually "transport is closing".
		case err == io.EOF:
			select {
			case c.StreamErrChan <- ErrServerShutdown:
			case <-c.quit:
			}
			return

		// Any other error is likely on a connection level and leaves
		// us no choice but to abort.
		case err != nil:
			select {
			case c.StreamErrChan <- err:
			case <-c.quit:
			}
			return
		}

		// We only handle two messages here, the initial challenge and
		// the shutdown. Everything else is passed into the channel to
		// be handled by a manager.
		switch t := msg.Msg.(type) {
		case *clmrpc.ServerAuctionMessage_Challenge:
			var (
				commitHash      [32]byte
				serverChallenge [32]byte
			)
			copy(commitHash[:], t.Challenge.CommitHash)
			var acctSub *acctSubscription
			for traderKey, sub := range c.subscribedAccts {
				if sub.commitHash == commitHash {
					acctSub = c.subscribedAccts[traderKey]
				}
			}
			if acctSub == nil {
				c.StreamErrChan <- fmt.Errorf("no sub"+
					"scription found for commit hash %x",
					commitHash)
				return
			}

			// Inform the subscription about the arrived challenge.
			copy(serverChallenge[:], t.Challenge.Challenge)
			select {
			case acctSub.challengeChan <- serverChallenge:
			case <-c.quit:
			}

		// The shutdown message is sent as a general error message. We
		// only handle this specific case here, the rest is forwarded to
		// the handler.
		case *clmrpc.ServerAuctionMessage_Error:
			errCode := t.Error.ErrorCode
			if errCode == clmrpc.SubscribeError_SERVER_SHUTDOWN {
				err := c.HandleServerShutdown(nil)
				if err != nil {
					select {
					case c.StreamErrChan <- err:
					case <-c.quit:
					}
				}
				return
			}

			// All other types of errors should be dealt with by the
			// handler.
			select {
			case c.FromServerChan <- msg:
			case <-c.quit:
			}

		// A valid message from the server. Forward it to the handler.
		default:
			select {
			case c.FromServerChan <- msg:
			case <-c.quit:
			}
		}
	}
}

// HandleServerShutdown handles the signal from the server that it is going to
// shut down. In that case, we try to reconnect a number of times with an
// incremental backoff time we wait between trials. If the connection succeeds,
// all previous subscriptions are sent again.
func (c *Client) HandleServerShutdown(err error) error {
	if err == nil {
		log.Infof("Server is shutting down, will reconnect in %v",
			c.cfg.MinBackoff)
	} else {
		log.Errorf("Error in stream, trying to reconnect: %v", err)
	}
	err = c.closeStream()
	if err != nil {
		log.Errorf("Error closing stream connection: %v", err)
	}

	// Guard the server stream from concurrent access. We can't use defer
	// to unlock here because SubscribeAccountUpdates is called later on
	// which requires access to the lock as well.
	c.streamMutex.Lock()
	err = c.connectServerStream(c.cfg.MinBackoff, reconnectRetries)
	if err != nil {
		c.streamMutex.Unlock()
		return err
	}
	c.streamMutex.Unlock()

	// With the connection re-established, check whether we need to mark our
	// pending batch as finalized, or if we need to remove it due to the
	// batch auction no longer including us.
	if err := c.checkPendingBatch(); err != nil {
		return err
	}

	// Subscribe to all accounts again. Remove the old subscriptions in the
	// same move as new ones will be created.
	acctKeys := make([]*keychain.KeyDescriptor, 0, len(c.subscribedAccts))
	for key, subscription := range c.subscribedAccts {
		acctKeys = append(acctKeys, subscription.acctKey)
		delete(c.subscribedAccts, key)
	}
	for _, acctKey := range acctKeys {
		err := c.SubscribeAccountUpdates(context.Background(), acctKey)
		if err != nil {
			return err
		}
	}
	return nil
}
