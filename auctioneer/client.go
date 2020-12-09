package auctioneer

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const (
	initialConnectRetries = 3
	reconnectRetries      = 10

	// maxUnusedAccountKeyLookup is the number of successive account keys
	// that we try and the server does not know of before aborting recovery.
	// This is necessary to skip "holes" in our list of keys that can happen
	// if the user tries to open an account but that fails. Then some keys
	// aren't used.
	maxUnusedAccountKeyLookup = 50
)

var (
	// ErrServerShutdown is the error that is returned if the auction server
	// signals it's going to shut down.
	ErrServerShutdown = errors.New("server shutting down")

	// ErrClientShutdown is the error that is returned if the trader client
	// itself is shutting down.
	ErrClientShutdown = errors.New("client shutting down")

	// ErrAuthCanceled is returned if the authentication process of a single
	// account subscription is aborted.
	ErrAuthCanceled = errors.New("authentication was canceled")
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

	// BatchCleaner provides functionality to clean up the state of a
	// trader's pending batch.
	BatchCleaner BatchCleaner
}

// Client performs the client side part of auctions. This interface exists to be
// able to implement a stub.
type Client struct {
	cfg *Config

	started uint32
	stopped uint32

	StreamErrChan  chan error
	errChanSwitch  *ErrChanSwitch
	FromServerChan chan *poolrpc.ServerAuctionMessage

	serverConn *grpc.ClientConn
	client     poolrpc.ChannelAuctioneerClient

	quit            chan struct{}
	wg              sync.WaitGroup
	serverStream    poolrpc.ChannelAuctioneer_SubscribeBatchAuctionClient
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

	mainErrChan := make(chan error)
	errChanSwitch := NewErrChanSwitch(mainErrChan)
	return &Client{
		cfg:             cfg,
		FromServerChan:  make(chan *poolrpc.ServerAuctionMessage),
		StreamErrChan:   mainErrChan,
		errChanSwitch:   errChanSwitch,
		quit:            make(chan struct{}),
		subscribedAccts: make(map[[33]byte]*acctSubscription),
	}, nil
}

// Start starts the client, establishing the connection to the server.
func (c *Client) Start() error {
	if !atomic.CompareAndSwapUint32(&c.started, 0, 1) {
		return nil
	}

	serverConn, err := grpc.Dial(c.cfg.ServerAddress, c.cfg.DialOpts...)
	if err != nil {
		return fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	c.serverConn = serverConn
	c.client = poolrpc.NewChannelAuctioneerClient(serverConn)

	c.errChanSwitch.Start()

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
	if !atomic.CompareAndSwapUint32(&c.stopped, 0, 1) {
		return nil
	}

	log.Infof("Shutting down auctioneer client")
	close(c.quit)
	err := c.closeStream()
	if err != nil {
		log.Errorf("Unable to close stream: %v", err)
	}
	c.wg.Wait()
	close(c.FromServerChan)
	c.errChanSwitch.Stop()
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
	c.serverStream = nil

	// Close all pending subscriptions.
	for _, subscription := range c.subscribedAccts {
		close(subscription.quit)
		close(subscription.msgChan)
	}

	return err
}

// ReserveAccount reserves an account with the auctioneer. It returns the base
// public key we should use for them in our 2-of-2 multi-sig construction, and
// the initial batch key.
func (c *Client) ReserveAccount(ctx context.Context, value btcutil.Amount,
	expiry uint32, traderKey *btcec.PublicKey) (*account.Reservation,
	error) {

	resp, err := c.client.ReserveAccount(ctx, &poolrpc.ReserveAccountRequest{
		AccountValue:  uint64(value),
		TraderKey:     traderKey.SerializeCompressed(),
		AccountExpiry: expiry,
	})
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

	_, err = c.client.InitAccount(ctx, &poolrpc.ServerInitAccountRequest{
		AccountPoint: &poolrpc.OutPoint{
			Txid:        account.OutPoint.Hash[:],
			OutputIndex: account.OutPoint.Index,
		},
		AccountScript: accountOutput.PkScript,
		AccountValue:  uint64(account.Value),
		AccountExpiry: account.Expiry,
		TraderKey:     account.TraderKey.PubKey.SerializeCompressed(),
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

	rpcInputs := make([]*poolrpc.ServerInput, 0, len(inputs))
	for _, input := range inputs {
		op := input.PreviousOutPoint
		rpcInputs = append(rpcInputs, &poolrpc.ServerInput{
			Outpoint: &poolrpc.OutPoint{
				Txid:        op.Hash[:],
				OutputIndex: op.Index,
			},
			SigScript: input.SignatureScript,
		})
	}

	rpcOutputs := make([]*poolrpc.ServerOutput, 0, len(outputs))
	for _, output := range outputs {
		rpcOutputs = append(rpcOutputs, &poolrpc.ServerOutput{
			Value:  uint64(output.Value),
			Script: output.PkScript,
		})
	}

	var rpcNewParams *poolrpc.ServerModifyAccountRequest_NewAccountParameters
	modifiedAccount := account.Copy(modifiers...)
	if len(modifiers) > 0 {
		rpcNewParams = &poolrpc.ServerModifyAccountRequest_NewAccountParameters{
			Value: uint64(modifiedAccount.Value),
		}
	}

	resp, err := c.client.ModifyAccount(ctx, &poolrpc.ServerModifyAccountRequest{
		TraderKey:  account.TraderKey.PubKey.SerializeCompressed(),
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
	rpcRequest := &poolrpc.ServerSubmitOrderRequest{}
	nodeAddrs := make([]*poolrpc.NodeAddress, 0, len(serverParams.Addrs))
	for _, addr := range serverParams.Addrs {
		nodeAddrs = append(nodeAddrs, &poolrpc.NodeAddress{
			Network: addr.Network(),
			Addr:    addr.String(),
		})
	}
	details := &poolrpc.ServerOrder{
		TraderKey:               o.Details().AcctKey[:],
		RateFixed:               o.Details().FixedRate,
		Amt:                     uint64(o.Details().Amt),
		MinChanAmt:              uint64(o.Details().MinUnitsMatch.ToSatoshis()),
		OrderNonce:              nonce[:],
		OrderSig:                serverParams.RawSig,
		MultiSigKey:             serverParams.MultiSigKey[:],
		NodePub:                 serverParams.NodePubkey[:],
		NodeAddr:                nodeAddrs,
		MaxBatchFeeRateSatPerKw: uint64(o.Details().MaxBatchFeeRate),
	}

	// Split into server message which is type specific.
	switch castOrder := o.(type) {
	case *order.Ask:
		serverAsk := &poolrpc.ServerAsk{
			Details:             details,
			LeaseDurationBlocks: castOrder.LeaseDuration,
			Version:             uint32(castOrder.Version),
		}
		rpcRequest.Details = &poolrpc.ServerSubmitOrderRequest_Ask{
			Ask: serverAsk,
		}

	case *order.Bid:
		nodeTierEnum, err := MarshallNodeTier(castOrder.MinNodeTier)
		if err != nil {
			return err
		}

		serverBid := &poolrpc.ServerBid{
			Details:             details,
			LeaseDurationBlocks: castOrder.LeaseDuration,
			Version:             uint32(castOrder.Version),
			MinNodeTier:         nodeTierEnum,
		}
		rpcRequest.Details = &poolrpc.ServerSubmitOrderRequest_Bid{
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
	case *poolrpc.ServerSubmitOrderResponse_InvalidOrder:
		return &order.UserError{
			FailMsg: submitResp.InvalidOrder.FailString,
			Details: submitResp.InvalidOrder,
		}

	case *poolrpc.ServerSubmitOrderResponse_Accepted:
		return nil

	default:
		return fmt.Errorf("unknown server response: %v", resp)
	}
}

// CancelOrder sends an order cancellation message to the server.
func (c *Client) CancelOrder(ctx context.Context, noncePreimage lntypes.Preimage) error {
	_, err := c.client.CancelOrder(ctx, &poolrpc.ServerCancelOrderRequest{
		OrderNoncePreimage: noncePreimage[:],
	})
	return err
}

// OrderState queries the state of an order on the server. This only returns the
// state as it's currently known to the server's database. For real-time updates
// on the state, the SubscribeBatchAuction stream should be used.
func (c *Client) OrderState(ctx context.Context, nonce order.Nonce) (
	*poolrpc.ServerOrderStateResponse, error) {

	return c.client.OrderState(ctx, &poolrpc.ServerOrderStateRequest{
		OrderNonce: nonce[:],
	})
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

	_, _, err := c.connectAndAuthenticate(ctx, acctKey, false)
	return err
}

// connectAndAuthenticate opens a stream to the server and authenticates the
// account to receive updates. It returns the subscription and a bool that
// indicates if recovery can be continued. That value can safely be ignored if
// recovery is not requested. Checking the returned error must take precedence
// to the boolean flag.
func (c *Client) connectAndAuthenticate(ctx context.Context,
	acctKey *keychain.KeyDescriptor, recovery bool) (*acctSubscription,
	bool, error) {

	var acctPubKey [33]byte
	copy(acctPubKey[:], acctKey.PubKey.SerializeCompressed())

	// Don't subscribe more than once.
	sub, ok := c.subscribedAccts[acctPubKey]
	if ok {
		if recovery {
			return sub, true, fmt.Errorf("account %x is already "+
				"subscribed, cannot recover", acctPubKey[:])
		}
		return sub, true, nil
	}

	c.streamMutex.Lock()
	defer c.streamMutex.Unlock()

	if c.serverStream == nil {
		err := c.connectServerStream(0, initialConnectRetries)
		if err != nil {
			return sub, false, fmt.Errorf("connecting server "+
				"stream failed: %v", err)
		}

		// Since this is the first time we establish our connection to
		// the auctioneer, check whether we need to mark our pending
		// batch as finalized, or if we need to remove it due to the
		// batch auction no longer including us.
		if err := c.checkPendingBatch(); err != nil {
			return sub, false, fmt.Errorf("checking pending "+
				"batch failed: %v", err)
		}
	}

	// For the duration this subscription is active, we need to redirect any
	// errors sent from the auctioneer to a different channel so the
	// subscription can react to it. Once we're done, we restore the
	// original channel.
	tempErrChan := make(chan error)
	c.errChanSwitch.Divert(tempErrChan)
	defer c.errChanSwitch.Restore()

	// Before we can expect to receive any updates, we need to perform the
	// 3-way authentication handshake.
	sub = &acctSubscription{
		acctKey: acctKey,
		sendMsg: c.SendAuctionMessage,
		signer:  c.cfg.Signer,
		msgChan: make(chan *poolrpc.ServerAuctionMessage),
		errChan: tempErrChan,
		quit:    make(chan struct{}),
	}
	c.subscribedAccts[acctPubKey] = sub
	err := sub.authenticate(ctx)
	if err != nil {
		return sub, false, err
	}

	// We always get a message back from the server. We can treat it
	// differently if we're in recovery mode though.
	select {
	case srvMsg := <-sub.msgChan:
		if srvMsg == nil {
			return sub, false, fmt.Errorf("no response received")
		}

		// Did the server find the account we're interested in?
		switch {
		// Account exists, everything's good to continue.
		case srvMsg.GetSuccess() != nil:
			return sub, true, nil

		// We got an error. If we're in recovery mode, this could either
		// mean the account doesn't exist or there's only a reservation
		// for it.
		case srvMsg.GetError() != nil:
			errMsg := srvMsg.GetError()

			// If we're not in recovery mode we don't expect any
			// error, this is a hard failure.
			if !recovery {
				return nil, false, fmt.Errorf("error "+
					"subscribing to account: %v",
					errMsg.Error)
			}

			// If we're in recovery mode, it is possible for an
			// account to still be in the reservation phase from the
			// point of view of the auctioneer. We cannot do the
			// normal recovery in that case because the account
			// subscription cannot be completed. This needs to be
			// handled specifically so we return a typed error now.
			if errMsg.ErrorCode == poolrpc.SubscribeError_INCOMPLETE_ACCOUNT_RESERVATION {
				return sub, true, AcctResNotCompletedErrFromRPC(
					*errMsg.AccountReservation,
				)
			}

			// The account doesn't exist. We are in recovery mode so
			// this is fine. We just skip this account key and try
			// the next one.
			if errMsg.ErrorCode == poolrpc.SubscribeError_ACCOUNT_DOES_NOT_EXIST {
				return sub, false, nil
			}

			return nil, false, fmt.Errorf("unknown error message "+
				"received: %v", errMsg)

		default:
			return nil, false, fmt.Errorf("unknown message "+
				"received: %v", srvMsg)
		}

	case err := <-tempErrChan:
		return nil, false, fmt.Errorf("error during authentication "+
			"when waiting for final step: %v", err)

	case <-sub.quit:
		return nil, false, ErrAuthCanceled

	case <-c.quit:
		return nil, false, ErrClientShutdown
	}
}

// RecoverAccounts tries to recover all given accounts with the help of the
// auctioneer server. Because the trader derives a new account key for each
// attempt of opening an account, there can be "holes" in our list of keys that
// are actually used. For example if there is insufficient balance in lnd, a key
// gets "used up" but no account is ever created with it. We'll do a sweep to
// ensure we generate a key only up to the point where it's required. The total
// number of requested recoveries is returned upon completion.
func (c *Client) RecoverAccounts(ctx context.Context,
	accountKeys []*keychain.KeyDescriptor) ([]*account.Account, error) {

	numNotFoundAccounts := 0
	var recoveredAccounts []*account.Account
	for _, keyDesc := range accountKeys {
		acctKeyBytes := keyDesc.PubKey.SerializeCompressed()
		var resErr *AcctResNotCompletedError

		// Start the recovery of this account by connecting and going
		// through the 3-way-handshake. There's four possibilities of
		// what can happen: 1. The account exists but only as a
		// reservation. This special case needs to be handled
		// separately. 2. We get an unexpected error in which case we
		// abort the recovery. 3. The account doesn't exist and we skip
		// it. 4. The account exists and we get a success message.
		// Normal recovery can be attempted.
		subscription, canRecover, err := c.connectAndAuthenticate(
			ctx, keyDesc, true,
		)

		// Case 1: Special recovery of an account that was only
		// reserved.
		if errors.As(err, &resErr) {
			acct, err := incompleteAcctFromErr(keyDesc, resErr)
			if err != nil {
				return nil, fmt.Errorf("could not recover "+
					"reserved account %x: %v", acctKeyBytes,
					err)
			}
			recoveredAccounts = append(recoveredAccounts, acct)

			// Don't send any further messages to the auctioneer,
			// we won't get any more information than was in the
			// error.
			continue
		}

		// Case 2: Abort on unexpected error.
		if err != nil {
			return nil, fmt.Errorf("could not recover account %x: "+
				"%v", acctKeyBytes, err)
		}

		// Case 3: The auctioneer doesn't know the account.
		if !canRecover {
			numNotFoundAccounts++

			// Stop looking for further accounts if we've got a
			// certain number of negative responses from the server.
			if numNotFoundAccounts > maxUnusedAccountKeyLookup {
				return recoveredAccounts, nil
			}

			continue
		}

		// Case 4: Attempt normal recovery. First we reset our not found
		// counter as we've found a recoverable account here. Then ask
		// the auctioneer to send us back the state as it knows it in
		// its database. The response to this message will be handled
		// outside of the client.
		numNotFoundAccounts = 0
		err = c.SendAuctionMessage(&poolrpc.ClientAuctionMessage{
			Msg: &poolrpc.ClientAuctionMessage_Recover{
				Recover: &poolrpc.AccountRecovery{
					TraderKey: acctKeyBytes,
				},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("error sending recover "+
				"request: %v", err)
		}

		// Now we wait for the server to send us the account to recover.
		select {
		case msg := <-subscription.msgChan:
			acctMsg, ok := msg.Msg.(*poolrpc.ServerAuctionMessage_Account)
			if !ok {
				return nil, fmt.Errorf("received unexpected "+
					"recovery message from server: %v",
					msg)
			}

			// The trader key must match our key, otherwise
			// something got out of order.
			acctKey, err := btcec.ParsePubKey(
				acctMsg.Account.TraderKey, btcec.S256(),
			)
			if err != nil {
				return nil, fmt.Errorf("error parsing account "+
					"key: %v", err)
			}
			if !acctKey.IsEqual(keyDesc.PubKey) {
				return nil, fmt.Errorf("invalid trader key, "+
					"got %x wanted %x",
					acctMsg.Account.TraderKey,
					keyDesc.PubKey.SerializeCompressed())
			}

			// Account is ok, parse the rest of the fields.
			acct, err := unmarshallServerRecoveredAccount(
				keyDesc, acctMsg.Account,
			)
			if err != nil {
				return nil, fmt.Errorf("error recovering "+
					"account: %v", err)
			}
			recoveredAccounts = append(recoveredAccounts, acct)

		case <-ctx.Done():
			return nil, fmt.Errorf("user canceled operation")

		case <-c.quit:
			return nil, ErrClientShutdown
		}
	}

	return recoveredAccounts, nil
}

// incompleteAcctFromErr creates an account in the state initiated from an error
// that contains the bare minimum of recovery information.
func incompleteAcctFromErr(traderKey *keychain.KeyDescriptor,
	resErr *AcctResNotCompletedError) (*account.Account, error) {

	var (
		acct = &account.Account{
			Value:      resErr.Value,
			State:      account.StateInitiated,
			TraderKey:  traderKey,
			Expiry:     resErr.Expiry,
			HeightHint: resErr.HeightHint,
		}
		err error
	)

	acct.AuctioneerKey, err = btcec.ParsePubKey(
		resErr.AuctioneerKey[:], btcec.S256(),
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing auctioneer key: %v", err)
	}

	acct.BatchKey, err = btcec.ParsePubKey(
		resErr.InitialBatchKey[:], btcec.S256(),
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing batch key: %v", err)
	}

	return acct, nil
}

// SendAuctionMessage sends an auction message through the long-lived stream to
// the auction server. A message can only be sent as a response to a server
// message, therefore the stream must already be open.
func (c *Client) SendAuctionMessage(msg *poolrpc.ClientAuctionMessage) error {
	if c.serverStream == nil {
		return fmt.Errorf("cannot send message, stream not open")
	}

	return c.serverStream.Send(msg)
}

// wait blocks for a given amount of time but returns immediately if the client
// is shutting down.
func (c *Client) wait(backoff time.Duration) error {
	select {
	case <-time.After(backoff):
		return nil

	case <-c.quit:
		return ErrClientShutdown
	}
}

// connectServerStream opens the initial connection to the server for the stream
// of account updates and handles reconnect trials with incremental backoff.
func (c *Client) connectServerStream(initialBackoff time.Duration,
	numRetries int) error {

	var (
		backoff = initialBackoff
		ctxb    = context.Background()
		ctx     context.Context
		err     error
	)
	for i := 0; i < numRetries; i++ {
		// Wait before connecting in case this is a reconnect trial.
		if backoff != 0 {
			err = c.wait(backoff)
			if err != nil {
				return err
			}
		}

		// Try connecting by querying a "cheap" RPC that the server can
		// answer from memory only.
		_, err = c.client.Terms(ctxb, &poolrpc.TermsRequest{})
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

		if i < numRetries-1 {
			log.Infof("Connection to server failed, will try again "+
				"in %v", backoff)
		}
	}
	if err != nil {
		log.Errorf("Connection to server failed after %d retries",
			numRetries)
		return err
	}

	// Now that we know the connection itself is established, we also re-
	// connect the long-lived stream.
	ctx, c.streamCancel = context.WithCancel(ctxb)
	c.serverStream, err = c.client.SubscribeBatchAuction(ctx)
	if err != nil {
		log.Errorf("Subscribing to batch auction failed: %v", err)
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
func (c *Client) readIncomingStream() { // nolint:gocyclo
	for {
		// Cancel the stream on client shutdown.
		select {
		case <-c.quit:
			return

		default:
		}

		// Read next message from server.
		msg, err := c.serverStream.Recv()
		log.Tracef("Received msg=%#v, err=%v from server", msg, err)
		switch {
		// EOF is the "normal" close signal, meaning the server has
		// cut its side of the connection. We will only get this during
		// the proper shutdown of the server where we already have a
		// reconnect scheduled. On an improper shutdown, we'll get an
		// error, usually "transport is closing".
		case err == io.EOF:
			select {
			case c.errChanSwitch.ErrChan() <- ErrServerShutdown:
			case <-c.quit:
			}
			return

		// Any other error is likely on a connection level and leaves
		// us no choice but to abort.
		case err != nil:
			// Context canceled is the error that signals we closed
			// the stream, most likely because the trader is
			// shutting down.
			s, ok := status.FromError(err)
			if ok && s.Code() == codes.Canceled {
				return
			}

			// Any other error we want to report back.
			select {
			case c.errChanSwitch.ErrChan() <- err:
			case <-c.quit:
			}
			return
		}

		// We only handle three kinds of messages here, those related to
		// the initial challenge, to the account recovery and the
		// shutdown. Everything else is passed into the channel to be
		// handled by a manager.
		switch t := msg.Msg.(type) {
		// The server sends us the challenge that we need to complete
		// the 3-way handshake.
		case *poolrpc.ServerAuctionMessage_Challenge:
			// Try to find the subscription this message is for so
			// we can send it over the correct chan.
			var commitHash [32]byte
			copy(commitHash[:], t.Challenge.CommitHash)
			var acctSub *acctSubscription
			for traderKey, sub := range c.subscribedAccts {
				if sub.commitHash == commitHash {
					acctSub = c.subscribedAccts[traderKey]
				}
			}
			if acctSub == nil {
				c.errChanSwitch.ErrChan() <- fmt.Errorf("no "+
					"subscription found for commit hash %x",
					commitHash)
				return
			}

			// Inform the subscription about the arrived challenge.
			select {
			case acctSub.msgChan <- msg:
			case <-c.quit:
			}

		// The server confirms the account subscription. We only really
		// care about this response in the recovery case because it
		// means we can recover this account.
		case *poolrpc.ServerAuctionMessage_Success:
			err := c.sendToSubscription(t.Success.TraderKey, msg)
			if err != nil {
				c.errChanSwitch.ErrChan() <- err
				return
			}

		// We've requested to recover an account and the auctioneer now
		// sent us their state of the account. We'll try to restore it
		// in our database.
		case *poolrpc.ServerAuctionMessage_Account:
			err := c.sendToSubscription(t.Account.TraderKey, msg)
			if err != nil {
				c.errChanSwitch.ErrChan() <- err
				return
			}

		// The shutdown message and the account not found error are sent
		// as general error messages. We only handle these two specific
		// cases here, the rest is forwarded to the handler.
		case *poolrpc.ServerAuctionMessage_Error:
			errCode := t.Error.ErrorCode

			switch errCode {
			// The server is shutting down. No need to forward this,
			// we can just shutdown the stream and try to reconnect.
			case poolrpc.SubscribeError_SERVER_SHUTDOWN:
				err := c.HandleServerShutdown(nil)
				if err != nil {
					select {
					case c.errChanSwitch.ErrChan() <- err:
					case <-c.quit:
					}
				}
				return

			// We received an account not found error. This is not
			// a reason to abort in case we are in recovery mode.
			// We let the subscription decide what to do.
			case poolrpc.SubscribeError_ACCOUNT_DOES_NOT_EXIST,
				poolrpc.SubscribeError_INCOMPLETE_ACCOUNT_RESERVATION:

				err := c.sendToSubscription(
					t.Error.TraderKey, msg,
				)
				if err != nil {
					c.errChanSwitch.ErrChan() <- err
					return
				}

				// Consider this handled, await the next message.
				continue
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

// sendToSubscription finds the subscription for a trader's account public key
// and forwards the given message to it.
func (c *Client) sendToSubscription(traderAccountKey []byte,
	msg *poolrpc.ServerAuctionMessage) error {

	// Try to find the subscription this message is for so we can send it
	// over the correct chan. If the key isn't a valid pubkey we'll just not
	// find it. All entries in the map are checked to be valid pubkeys when
	// added.
	var traderKey [33]byte
	copy(traderKey[:], traderAccountKey)
	acctSub, ok := c.subscribedAccts[traderKey]
	if !ok {
		return fmt.Errorf("no subscription found for account key %x",
			traderAccountKey)
	}

	// Inform the subscription about the arrived message.
	select {
	case acctSub.msgChan <- msg:
	case <-c.quit:
	}

	return nil
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
	c.streamMutex.Unlock()
	if err != nil {
		return err
	}

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

// unmarshallServerAccount parses the account information sent from the
// auctioneer into our local account struct.
func unmarshallServerRecoveredAccount(keyDesc *keychain.KeyDescriptor,
	a *poolrpc.AuctionAccount) (*account.Account, error) {

	// Parse all raw public keys.
	auctioneerKey, err := btcec.ParsePubKey(a.AuctioneerKey, btcec.S256())
	if err != nil {
		return nil, fmt.Errorf("error parsing auctioneer key: %v", err)
	}
	batchKey, err := btcec.ParsePubKey(a.BatchKey, btcec.S256())
	if err != nil {
		return nil, fmt.Errorf("error parsing batch key: %v", err)
	}

	// The auctioneer doesn't track the account in the same granularity as
	// we do in the trader. Therefore we need to map them back with caution.
	// The auctioneer might think a state is finalized but we want to wait
	// for the confirmation on chain in any case. That's why we always map
	// the auctioneer's state to the corresponding pending state on our
	// side.
	state := account.StateClosed
	switch a.State {
	case poolrpc.AuctionAccountState_STATE_OPEN,
		poolrpc.AuctionAccountState_STATE_PENDING_OPEN:

		state = account.StateInitiated

	case poolrpc.AuctionAccountState_STATE_CLOSED:
		state = account.StatePendingClosed

	case poolrpc.AuctionAccountState_STATE_PENDING_UPDATE:
		state = account.StatePendingUpdate

	case poolrpc.AuctionAccountState_STATE_PENDING_BATCH:
		state = account.StatePendingBatch

	case poolrpc.AuctionAccountState_STATE_EXPIRED:
		state = account.StateExpired
	}

	// Parse the rest of the more complex values.
	if a.Outpoint == nil {
		return nil, fmt.Errorf("account outpoint is missing")
	}
	hash, err := chainhash.NewHash(a.Outpoint.Txid)
	if err != nil {
		return nil, fmt.Errorf("error parsing outpoint hash: %v", err)
	}

	// The latest transaction is only known to the auctioneer after it's
	// been confirmed.
	var latestTx *wire.MsgTx
	if a.State != poolrpc.AuctionAccountState_STATE_PENDING_OPEN {
		latestTx = &wire.MsgTx{}
		err := latestTx.Deserialize(bytes.NewReader(a.LatestTx))
		if err != nil {
			return nil, err
		}
	}

	return &account.Account{
		Value:         btcutil.Amount(a.Value),
		Expiry:        a.Expiry,
		TraderKey:     keyDesc,
		AuctioneerKey: auctioneerKey,
		BatchKey:      batchKey,
		State:         state,
		HeightHint:    a.HeightHint,
		OutPoint: wire.OutPoint{
			Hash:  *hash,
			Index: a.Outpoint.OutputIndex,
		},
		LatestTx: latestTx,
	}, nil
}

// Terms returns the current dynamic auctioneer terms like max account size, max
// order duration in blocks and the auction fee schedule.
func (c *Client) Terms(ctx context.Context) (*terms.AuctioneerTerms, error) {
	resp, err := c.client.Terms(ctx, &poolrpc.TermsRequest{})
	if err != nil {
		return nil, err
	}

	return &terms.AuctioneerTerms{
		MaxAccountValue:     btcutil.Amount(resp.MaxAccountValue),
		MaxOrderDuration:    resp.MaxOrderDurationBlocks,
		OrderExecBaseFee:    btcutil.Amount(resp.ExecutionFee.BaseFee),
		OrderExecFeeRate:    btcutil.Amount(resp.ExecutionFee.FeeRate),
		LeaseDurations:      resp.LeaseDurations,
		NextBatchConfTarget: resp.NextBatchConfTarget,
		NextBatchFeeRate:    chainfee.SatPerKWeight(resp.NextBatchFeeRateSatPerKw),
		NextBatchClear:      time.Unix(int64(resp.NextBatchClearTimestamp), 0),
	}, nil
}

// BatchSnapshot returns information about a target batch including the
// clearing price of the batch, and the set of orders matched within the batch.
//
// NOTE: This isn't wrapped in "native" types, as atm we only use this to
// shuffle information back to the client over our RPC interface.
func (c *Client) BatchSnapshot(ctx context.Context,
	targetBatch order.BatchID) (*poolrpc.BatchSnapshotResponse, error) {

	return c.client.BatchSnapshot(ctx, &poolrpc.BatchSnapshotRequest{
		BatchId: targetBatch[:],
	})
}

// BatchSnapshots returns a list of batch snapshots starting at the start batch
// ID and going back through the history of batches, returning at most the
// number of specified batches. A maximum of 100 snapshots can be queried in
// one call. If no start batch ID is provided, the most recent finalized batch
// is used as the starting point to go back from.
//
// NOTE: This isn't wrapped in "native" types, as atm we only use this to
// shuffle information back to the client over our RPC interface.
func (c *Client) BatchSnapshots(ctx context.Context,
	req *poolrpc.BatchSnapshotsRequest) (*poolrpc.BatchSnapshotsResponse,
	error) {

	return c.client.BatchSnapshots(ctx, req)
}

// NodeRating returns the current up to date ratings information for the target
// node pubkey.
func (c *Client) NodeRating(ctx context.Context,
	nodeKeys ...*btcec.PublicKey) (*poolrpc.NodeRatingResponse, error) {

	pubKeys := make([][]byte, 0, len(nodeKeys))
	for _, nodeKey := range nodeKeys {
		pubKeys = append(pubKeys, nodeKey.SerializeCompressed())
	}

	req := &poolrpc.ServerNodeRatingRequest{
		NodePubkeys: pubKeys,
	}
	serverResp, err := c.client.NodeRating(ctx, req)
	if err != nil {
		return nil, err
	}

	return &poolrpc.NodeRatingResponse{
		NodeRatings: serverResp.NodeRatings,
	}, nil
}

// MarshallNodeTier maps the node tier integer into the enum used on the RPC
// interface.
func MarshallNodeTier(nodeTier order.NodeTier) (poolrpc.NodeTier, error) {
	switch nodeTier {
	case order.NodeTierDefault:
		return poolrpc.NodeTier_TIER_DEFAULT, nil

	case order.NodeTier0:
		return poolrpc.NodeTier_TIER_0, nil

	case order.NodeTier1:
		return poolrpc.NodeTier_TIER_1, nil

	default:
		return 0, fmt.Errorf("unknown node tier: %v", nodeTier)
	}
}
