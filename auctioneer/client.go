package auctioneer

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/tor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const (
	reconnectRetries = math.MaxInt16

	// MaxUnusedAccountKeyLookup is the number of successive account keys
	// that we try and the server does not know of before aborting recovery.
	// This is necessary to skip "holes" in our list of keys that can happen
	// if the user tries to open an account but that fails. Then some keys
	// aren't used.
	MaxUnusedAccountKeyLookup = 50
)

var (
	// ErrServerShutdown is the error that is returned if the auction server
	// signals it's going to shut down.
	ErrServerShutdown = errors.New("server shutting down")

	// ErrServerErrored is the error that is returned if the auction server
	// sends back an error instead of a proper message.
	ErrServerErrored = errors.New("server sent unexpected error")

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

	// ProxyAddress is the SOCKS proxy that should be used to establish the
	// connection.
	ProxyAddress string

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

	// BatchVersion is the batch version that we should use when
	// authenticating with the auction server.
	BatchVersion order.BatchVersion

	// GenUserAgent is a function that generates a complete user agent
	// string given the incoming request context.
	GenUserAgent func(context.Context) string

	// ConnectSidecar is a flag indicating that instead of connecting to the
	// default SubscribeBatchAuction RPC the client should connect to
	// SubscribeSidecar for getting batch updates.
	ConnectSidecar bool
}

// Client performs the client side part of auctions. This interface exists to be
// able to implement a stub.
type Client struct {
	cfg *Config

	started uint32
	stopped uint32

	StreamErrChan  chan error
	errChanSwitch  *ErrChanSwitch
	FromServerChan chan *auctioneerrpc.ServerAuctionMessage

	serverConn     *grpc.ClientConn
	client         auctioneerrpc.ChannelAuctioneerClient
	hashMailClient auctioneerrpc.HashMailClient

	quit         chan struct{}
	wg           sync.WaitGroup
	serverStream auctioneerrpc.ChannelAuctioneer_SubscribeBatchAuctionClient
	streamMutex  sync.Mutex
	streamCancel func()

	subscribedAccts    map[[33]byte]*acctSubscription
	subscribedAcctsMtx sync.Mutex
}

// NewClient returns a new instance to initiate auctions with.
func NewClient(cfg *Config) (*Client, error) {
	var err error
	cfg.DialOpts, err = getAuctionServerDialOpts(
		cfg.Insecure, cfg.ProxyAddress, cfg.TLSPathServer, cfg.DialOpts...,
	)
	if err != nil {
		return nil, err
	}

	mainErrChan := make(chan error)
	errChanSwitch := NewErrChanSwitch(mainErrChan)
	return &Client{
		cfg:             cfg,
		FromServerChan:  make(chan *auctioneerrpc.ServerAuctionMessage),
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
	c.client = auctioneerrpc.NewChannelAuctioneerClient(serverConn)
	c.hashMailClient = auctioneerrpc.NewHashMailClient(serverConn)

	c.errChanSwitch.Start()

	return nil
}

// getAuctionServerDialOpts returns the dial options to connect to the auction
// server.
func getAuctionServerDialOpts(insecure bool, proxyAddress, tlsPath string,
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
	// If a SOCKS proxy address was specified,
	// then we should dial through it.
	if proxyAddress != "" {
		log.Infof("Proxying connection to auction server "+
			"over Tor SOCKS proxy %v",
			proxyAddress)
		torDialer := func(_ context.Context, addr string) (net.Conn, error) {
			return tor.Dial(
				addr, proxyAddress, false, false,
				tor.DefaultConnTimeout,
			)
		}
		opts = append(opts, grpc.WithContextDialer(torDialer))
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
	c.subscribedAcctsMtx.Lock()
	for _, subscription := range c.subscribedAccts {
		close(subscription.quit)
		close(subscription.msgChan)
	}
	c.subscribedAcctsMtx.Unlock()

	return err
}

// ReserveAccount reserves an account with the auctioneer. It returns the base
// public key we should use for them in our 2-of-2 multi-sig construction, and
// the initial batch key.
func (c *Client) ReserveAccount(ctx context.Context, value btcutil.Amount,
	expiry uint32, traderKey *btcec.PublicKey,
	version account.Version) (*account.Reservation, error) {

	resp, err := c.client.ReserveAccount(
		ctx, &auctioneerrpc.ReserveAccountRequest{
			AccountValue:  uint64(value),
			TraderKey:     traderKey.SerializeCompressed(),
			AccountExpiry: expiry,
			Version:       uint32(version),
		},
	)
	if err != nil {
		return nil, err
	}

	auctioneerKey, err := btcec.ParsePubKey(resp.AuctioneerKey)
	if err != nil {
		return nil, err
	}
	initialBatchKey, err := btcec.ParsePubKey(resp.InitialBatchKey)
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

	_, err = c.client.InitAccount(
		ctx, &auctioneerrpc.ServerInitAccountRequest{
			AccountPoint: &auctioneerrpc.OutPoint{
				Txid:        account.OutPoint.Hash[:],
				OutputIndex: account.OutPoint.Index,
			},
			AccountScript: accountOutput.PkScript,
			AccountValue:  uint64(account.Value),
			AccountExpiry: account.Expiry,
			TraderKey:     account.TraderKey.PubKey.SerializeCompressed(),
			UserAgent:     c.cfg.GenUserAgent(ctx),
			Version:       uint32(account.Version),
		},
	)
	return err
}

// ModifyAccount sends an intent to the auctioneer that we'd like to modify the
// account with the associated trader key. The auctioneer's signature is
// returned, allowing us to broadcast a transaction spending from the account
// allowing our modifications to take place. If the account spend is a MuSig2
// spend, then the trader's nonces must be sent and the server's nonces are
// returned upon success. The inputs and outputs provided should exclude the
// account input being spent and the account output potentially being recreated,
// since the auctioneer can construct those themselves. If no modifiers are
// present, then the auctioneer will interpret the request as an account
// closure. The previous outputs must always contain the UTXO information for
// _every_ input of the transaction, so inputs+account_input.
func (c *Client) ModifyAccount(ctx context.Context, account *account.Account,
	inputs []*wire.TxIn, outputs []*wire.TxOut,
	modifiers []account.Modifier, traderNonces []byte,
	previousOutputs []*wire.TxOut) ([]byte, []byte, error) {

	rpcInputs := make([]*auctioneerrpc.ServerInput, 0, len(inputs))
	for _, input := range inputs {
		op := input.PreviousOutPoint
		rpcInputs = append(rpcInputs, &auctioneerrpc.ServerInput{
			Outpoint: &auctioneerrpc.OutPoint{
				Txid:        op.Hash[:],
				OutputIndex: op.Index,
			},
			SigScript: input.SignatureScript,
		})
	}

	rpcOutputs := make([]*auctioneerrpc.ServerOutput, 0, len(outputs))
	for _, output := range outputs {
		rpcOutputs = append(rpcOutputs, &auctioneerrpc.ServerOutput{
			Value:  uint64(output.Value),
			Script: output.PkScript,
		})
	}

	req := &auctioneerrpc.ServerModifyAccountRequest{
		TraderKey:    account.TraderKey.PubKey.SerializeCompressed(),
		NewInputs:    rpcInputs,
		NewOutputs:   rpcOutputs,
		TraderNonces: traderNonces,
		PrevOutputs:  make([]*auctioneerrpc.TxOut, len(previousOutputs)),
	}

	if len(modifiers) > 0 {
		modifiedAccount := account.Copy(modifiers...)
		req.NewParams = &auctioneerrpc.ServerModifyAccountRequest_NewAccountParameters{
			Value:   uint64(modifiedAccount.Value),
			Expiry:  modifiedAccount.Expiry,
			Version: uint32(modifiedAccount.Version),
		}
	}

	for idx, prevOutput := range previousOutputs {
		req.PrevOutputs[idx] = &auctioneerrpc.TxOut{
			Value:    uint64(prevOutput.Value),
			PkScript: prevOutput.PkScript,
		}
	}

	resp, err := c.client.ModifyAccount(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	return resp.AccountSig, resp.ServerNonces, nil
}

// SubmitOrder sends a fully finished order message to the server and interprets
// its answer.
func (c *Client) SubmitOrder(ctx context.Context, o order.Order,
	serverParams *order.ServerOrderParams) error {

	// Prepare everything that is common to both ask and bid orders.
	nonce := o.Nonce()
	rpcRequest := &auctioneerrpc.ServerSubmitOrderRequest{
		UserAgent: c.cfg.GenUserAgent(ctx),
	}
	nodeAddrs := make([]*auctioneerrpc.NodeAddress, 0, len(serverParams.Addrs))
	for _, addr := range serverParams.Addrs {
		nodeAddrs = append(nodeAddrs, &auctioneerrpc.NodeAddress{
			Network: addr.Network(),
			Addr:    addr.String(),
		})
	}

	var channelType auctioneerrpc.OrderChannelType
	switch o.Details().ChannelType {
	case order.ChannelTypePeerDependent:
		channelType = auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_PEER_DEPENDENT
	case order.ChannelTypeScriptEnforced:
		channelType = auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_SCRIPT_ENFORCED
	default:
		return fmt.Errorf("unhandled channel type %v", c)
	}

	var auctionType auctioneerrpc.AuctionType
	switch o.Details().AuctionType {
	case order.BTCInboundLiquidity:
		auctionType = auctioneerrpc.AuctionType_AUCTION_TYPE_BTC_INBOUND_LIQUIDITY
	case order.BTCOutboundLiquidity:
		auctionType = auctioneerrpc.AuctionType_AUCTION_TYPE_BTC_OUTBOUND_LIQUIDITY
	}
	minChanAmt := uint64(o.Details().MinUnitsMatch.ToSatoshis())

	details := &auctioneerrpc.ServerOrder{
		TraderKey:               o.Details().AcctKey[:],
		AuctionType:             auctionType,
		RateFixed:               o.Details().FixedRate,
		Amt:                     uint64(o.Details().Amt),
		MinChanAmt:              minChanAmt,
		OrderNonce:              nonce[:],
		OrderSig:                serverParams.RawSig,
		MultiSigKey:             serverParams.MultiSigKey[:],
		NodePub:                 serverParams.NodePubkey[:],
		NodeAddr:                nodeAddrs,
		ChannelType:             channelType,
		MaxBatchFeeRateSatPerKw: uint64(o.Details().MaxBatchFeeRate),
		IsPublic:                o.Details().IsPublic,
	}

	details.AllowedNodeIds = make(
		[][]byte, len(o.Details().AllowedNodeIDs),
	)
	for idx, nodeID := range o.Details().AllowedNodeIDs {
		details.AllowedNodeIds[idx] = make([]byte, len(nodeID))
		copy(details.AllowedNodeIds[idx], nodeID[:])
	}

	details.NotAllowedNodeIds = make(
		[][]byte, len(o.Details().NotAllowedNodeIDs),
	)
	for idx, nodeID := range o.Details().NotAllowedNodeIDs {
		details.NotAllowedNodeIds[idx] = make([]byte, len(nodeID))
		copy(details.NotAllowedNodeIds[idx], nodeID[:])
	}

	// Split into server message which is type specific.
	switch castOrder := o.(type) {
	case *order.Ask:
		announcement := auctioneerrpc.ChannelAnnouncementConstraints(
			castOrder.AnnouncementConstraints,
		)
		confirmations := auctioneerrpc.ChannelConfirmationConstraints(
			castOrder.ConfirmationConstraints,
		)
		serverAsk := &auctioneerrpc.ServerAsk{
			Details:                 details,
			LeaseDurationBlocks:     castOrder.LeaseDuration,
			Version:                 uint32(castOrder.Version),
			AnnouncementConstraints: announcement,
			ConfirmationConstraints: confirmations,
		}
		rpcRequest.Details = &auctioneerrpc.ServerSubmitOrderRequest_Ask{
			Ask: serverAsk,
		}

	case *order.Bid:
		nodeTierEnum, err := MarshallNodeTier(castOrder.MinNodeTier)
		if err != nil {
			return err
		}

		serverBid := &auctioneerrpc.ServerBid{
			Details:             details,
			LeaseDurationBlocks: castOrder.LeaseDuration,
			Version:             uint32(castOrder.Version),
			MinNodeTier:         nodeTierEnum,
			SelfChanBalance:     uint64(castOrder.SelfChanBalance),
			IsSidecarChannel:    castOrder.SidecarTicket != nil,
			UnannouncedChannel:  castOrder.UnannouncedChannel,
			ZeroConfChannel:     castOrder.ZeroConfChannel,
		}
		rpcRequest.Details = &auctioneerrpc.ServerSubmitOrderRequest_Bid{
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
	case *auctioneerrpc.ServerSubmitOrderResponse_InvalidOrder:
		return &order.UserError{
			FailMsg: submitResp.InvalidOrder.FailString,
			Details: submitResp.InvalidOrder,
		}

	case *auctioneerrpc.ServerSubmitOrderResponse_Accepted:
		return nil

	default:
		return fmt.Errorf("unknown server response: %v", resp)
	}
}

// CancelOrder sends an order cancellation message to the server.
func (c *Client) CancelOrder(ctx context.Context, noncePreimage lntypes.Preimage) error {
	_, err := c.client.CancelOrder(
		ctx, &auctioneerrpc.ServerCancelOrderRequest{
			OrderNoncePreimage: noncePreimage[:],
		},
	)
	return err
}

// OrderState queries the state of an order on the server. This only returns the
// state as it's currently known to the server's database. For real-time updates
// on the state, the SubscribeBatchAuction stream should be used.
func (c *Client) OrderState(ctx context.Context, nonce order.Nonce) (
	*auctioneerrpc.ServerOrderStateResponse, error) {

	return c.client.OrderState(ctx, &auctioneerrpc.ServerOrderStateRequest{
		OrderNonce: nonce[:],
	})
}

// StartAccountSubscription opens a stream to the server and subscribes to all
// updates that concern the given account, including all orders that spend from
// that account. Only a single stream is ever open to the server, so a second
// call to this method will send a second subscription over the same stream,
// multiplexing all messages into the same connection. A stream can be
// long-lived, so this can be called for every account as soon as it's confirmed
// open. This method will return as soon as the authentication was successful.
// Messages sent from the server can then be received on the FromServerChan
// channel.
func (c *Client) StartAccountSubscription(ctx context.Context,
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
	c.subscribedAcctsMtx.Lock()
	sub, ok := c.subscribedAccts[acctPubKey]
	c.subscribedAcctsMtx.Unlock()
	if ok {
		if recovery {
			return sub, true, fmt.Errorf("account %x is already "+
				"subscribed, cannot recover", acctPubKey[:])
		}
		return sub, true, nil
	}

	// Guard the read access only. We can't use defer for the unlock because
	// both the connectServerStream and SendAuctionMessage need to hold the
	// mutex as well.
	c.streamMutex.Lock()
	needToConnect := c.serverStream == nil
	c.streamMutex.Unlock()

	if needToConnect {
		err := c.connectServerStream(0, reconnectRetries)
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
		acctKey:      acctKey,
		sendMsg:      c.SendAuctionMessage,
		signer:       c.cfg.Signer,
		msgChan:      make(chan *auctioneerrpc.ServerAuctionMessage),
		batchVersion: c.cfg.BatchVersion,
		errChan:      tempErrChan,
		quit:         make(chan struct{}),
	}
	c.subscribedAcctsMtx.Lock()
	c.subscribedAccts[acctPubKey] = sub
	c.subscribedAcctsMtx.Unlock()
	err := sub.authenticate(ctx)
	if err != nil {
		log.Errorf("Authentication failed for account %x: %v",
			acctPubKey[:], err)

		// The error that's returned from authenticate() might just be
		// an error that occurred when sending on the stream. If the
		// server is timing us out (or is shutting down) before we can
		// send the final message, this might not be the _cause_ of the
		// problem but just a follow-up symptom. So we need to look if
		// there's anything on the tempErrChan (which we write to when
		// getting unexpected messages or errors from the server) that's
		// more conclusive.
		select {
		case err := <-tempErrChan:
			// Ah, so it's the server shutting down, so let's re-
			// try our connection.
			if err == ErrServerErrored {
				return sub, false, c.HandleServerShutdown(nil)
			}

		default:
		}
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
			if errMsg.ErrorCode == auctioneerrpc.SubscribeError_INCOMPLETE_ACCOUNT_RESERVATION {
				return sub, true, AcctResNotCompletedErrFromRPC(
					errMsg.AccountReservation,
				)
			}

			// The account doesn't exist. We are in recovery mode so
			// this is fine. We just skip this account key and try
			// the next one.
			if errMsg.ErrorCode == auctioneerrpc.SubscribeError_ACCOUNT_DOES_NOT_EXIST {
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
			if numNotFoundAccounts > MaxUnusedAccountKeyLookup {
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
		err = c.SendAuctionMessage(&auctioneerrpc.ClientAuctionMessage{
			Msg: &auctioneerrpc.ClientAuctionMessage_Recover{
				Recover: &auctioneerrpc.AccountRecovery{
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
			acctMsg, ok := msg.Msg.(*auctioneerrpc.ServerAuctionMessage_Account)
			if !ok {
				return nil, fmt.Errorf("received unexpected "+
					"recovery message from server: %v",
					msg)
			}

			// The trader key must match our key, otherwise
			// something got out of order.
			acctKey, err := btcec.ParsePubKey(
				acctMsg.Account.TraderKey,
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
			Version:    account.Version(resErr.Version),
		}
		err error
	)

	acct.AuctioneerKey, err = btcec.ParsePubKey(resErr.AuctioneerKey[:])
	if err != nil {
		return nil, fmt.Errorf("error parsing auctioneer key: %v", err)
	}

	acct.BatchKey, err = btcec.ParsePubKey(resErr.InitialBatchKey[:])
	if err != nil {
		return nil, fmt.Errorf("error parsing batch key: %v", err)
	}

	return acct, nil
}

// SendAuctionMessage sends an auction message through the long-lived stream to
// the auction server. A message can only be sent as a response to a server
// message, therefore the stream must already be open.
func (c *Client) SendAuctionMessage(msg *auctioneerrpc.ClientAuctionMessage) error {
	c.streamMutex.Lock()
	defer c.streamMutex.Unlock()

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

// IsSubscribed returns true if at least one account is in an active state and
// the subscription stream to the server was established successfully.
func (c *Client) IsSubscribed() bool {
	c.streamMutex.Lock()
	defer c.streamMutex.Unlock()

	return c.serverStream != nil
}

// connectServerStream opens the initial connection to the server for the stream
// of account updates and handles reconnect trials with incremental backoff.
func (c *Client) connectServerStream(initialBackoff time.Duration,
	numRetries int) error {

	c.streamMutex.Lock()
	defer c.streamMutex.Unlock()

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
		_, err = c.client.Terms(ctxb, &auctioneerrpc.TermsRequest{})
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
	if c.cfg.ConnectSidecar {
		c.serverStream, err = c.client.SubscribeSidecar(ctx)
	} else {
		c.serverStream, err = c.client.SubscribeBatchAuction(ctx)
	}
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

		// This should never happen as we always close the quit channel
		// before we set the connection to nil (which _should_ cause us
		// to return in the first place), but just to be safe and avoid
		// a panic.
		if c.serverStream == nil {
			return
		}

		// Read next message from server.
		msg, err := c.serverStream.Recv()
		log.Tracef("Received msg=%v, err=%v from server",
			poolrpc.PrintMsg(msg), err)

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

			log.Errorf("Server connection error received: %v", err)

			// For any other error type, we'll attempt to trigger
			// the reconnect logic so we'll always try to connect
			// to the server in the background.
			select {
			case c.errChanSwitch.ErrChan() <- ErrServerErrored:
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
		case *auctioneerrpc.ServerAuctionMessage_Challenge:
			// Try to find the subscription this message is for so
			// we can send it over the correct chan.
			var commitHash [32]byte
			copy(commitHash[:], t.Challenge.CommitHash)
			var acctSub *acctSubscription
			c.subscribedAcctsMtx.Lock()
			for traderKey, sub := range c.subscribedAccts {
				if sub.commitHash == commitHash {
					acctSub = c.subscribedAccts[traderKey]
				}
			}
			c.subscribedAcctsMtx.Unlock()
			if acctSub == nil {
				select {
				case c.errChanSwitch.ErrChan() <- fmt.Errorf(
					"no subscription found for commit "+
						"hash %x", commitHash):
				case <-c.quit:
				}
				return
			}

			// Inform the subscription about the arrived challenge.
			select {
			case acctSub.msgChan <- msg:
			case <-c.quit:
				return
			}

		// The server confirms the account subscription. We only really
		// care about this response in the recovery case because it
		// means we can recover this account.
		case *auctioneerrpc.ServerAuctionMessage_Success:
			err := c.sendToSubscription(t.Success.TraderKey, msg)
			if err != nil {
				select {
				case c.errChanSwitch.ErrChan() <- err:
				case <-c.quit:
				}
				return
			}

		// We've requested to recover an account and the auctioneer now
		// sent us their state of the account. We'll try to restore it
		// in our database.
		case *auctioneerrpc.ServerAuctionMessage_Account:
			err := c.sendToSubscription(t.Account.TraderKey, msg)
			if err != nil {
				select {
				case c.errChanSwitch.ErrChan() <- err:
				case <-c.quit:
				}
				return
			}

		// The shutdown message and the account not found error are sent
		// as general error messages. We only handle these two specific
		// cases here, the rest is forwarded to the handler.
		case *auctioneerrpc.ServerAuctionMessage_Error:
			errCode := t.Error.ErrorCode

			switch errCode {
			// The server is shutting down. No need to forward this,
			// we can just shutdown the stream and try to reconnect.
			case auctioneerrpc.SubscribeError_SERVER_SHUTDOWN:
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
			case auctioneerrpc.SubscribeError_ACCOUNT_DOES_NOT_EXIST,
				auctioneerrpc.SubscribeError_INCOMPLETE_ACCOUNT_RESERVATION:

				err := c.sendToSubscription(
					t.Error.TraderKey, msg,
				)
				if err != nil {
					select {
					case c.errChanSwitch.ErrChan() <- err:
					case <-c.quit:
					}
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
				return
			}

		// A valid message from the server. Forward it to the handler.
		default:
			select {
			case c.FromServerChan <- msg:
			case <-c.quit:
				return
			}
		}
	}
}

// sendToSubscription finds the subscription for a trader's account public key
// and forwards the given message to it.
func (c *Client) sendToSubscription(traderAccountKey []byte,
	msg *auctioneerrpc.ServerAuctionMessage) error {

	// Try to find the subscription this message is for so we can send it
	// over the correct chan. If the key isn't a valid pubkey we'll just not
	// find it. All entries in the map are checked to be valid pubkeys when
	// added.
	var traderKey [33]byte
	copy(traderKey[:], traderAccountKey)
	c.subscribedAcctsMtx.Lock()
	acctSub, ok := c.subscribedAccts[traderKey]
	c.subscribedAcctsMtx.Unlock()
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

	// Try to get a new connection, retry if not successful immediately.
	err = c.connectServerStream(c.cfg.MinBackoff, reconnectRetries)
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
	c.subscribedAcctsMtx.Lock()
	acctKeys := make([]*keychain.KeyDescriptor, 0, len(c.subscribedAccts))
	for key, subscription := range c.subscribedAccts {
		acctKeys = append(acctKeys, subscription.acctKey)
		delete(c.subscribedAccts, key)
	}
	c.subscribedAcctsMtx.Unlock()
	for _, acctKey := range acctKeys {
		err := c.StartAccountSubscription(context.Background(), acctKey)
		if err != nil {
			return err
		}
	}
	return nil
}

// unmarshallServerAccount parses the account information sent from the
// auctioneer into our local account struct.
func unmarshallServerRecoveredAccount(keyDesc *keychain.KeyDescriptor,
	a *auctioneerrpc.AuctionAccount) (*account.Account, error) {

	// Parse all raw public keys.
	auctioneerKey, err := btcec.ParsePubKey(a.AuctioneerKey)
	if err != nil {
		return nil, fmt.Errorf("error parsing auctioneer key: %v", err)
	}
	batchKey, err := btcec.ParsePubKey(a.BatchKey)
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
	case auctioneerrpc.AuctionAccountState_STATE_OPEN,
		auctioneerrpc.AuctionAccountState_STATE_PENDING_OPEN:

		state = account.StateInitiated

	case auctioneerrpc.AuctionAccountState_STATE_CLOSED:
		// The auctioneer will keep the state as STATE_OPEN until the
		// closing TX confirms on chain. Therefore if we get the
		// STATE_CLOSED here it's already confirmed and we don't need to
		// watch anything or try to re-publish the closing TX.
		state = account.StateClosed

	case auctioneerrpc.AuctionAccountState_STATE_PENDING_UPDATE:
		state = account.StatePendingUpdate

	case auctioneerrpc.AuctionAccountState_STATE_PENDING_BATCH:
		state = account.StatePendingBatch

	case auctioneerrpc.AuctionAccountState_STATE_EXPIRED:
		state = account.StateExpired

	case auctioneerrpc.AuctionAccountState_STATE_EXPIRED_PENDING_UPDATE:
		state = account.StateExpiredPendingUpdate
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
	if a.State != auctioneerrpc.AuctionAccountState_STATE_PENDING_OPEN {
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
		Version:  account.Version(a.Version),
	}, nil
}

// Terms returns the current dynamic auctioneer terms like max account size, max
// order duration in blocks and the auction fee schedule.
func (c *Client) Terms(ctx context.Context) (*terms.AuctioneerTerms, error) {
	resp, err := c.client.Terms(ctx, &auctioneerrpc.TermsRequest{})
	if err != nil {
		return nil, err
	}

	return &terms.AuctioneerTerms{
		MaxAccountValue:          btcutil.Amount(resp.MaxAccountValue),
		OrderExecBaseFee:         btcutil.Amount(resp.ExecutionFee.BaseFee),
		OrderExecFeeRate:         btcutil.Amount(resp.ExecutionFee.FeeRate),
		LeaseDurationBuckets:     resp.LeaseDurationBuckets,
		NextBatchConfTarget:      resp.NextBatchConfTarget,
		NextBatchFeeRate:         chainfee.SatPerKWeight(resp.NextBatchFeeRateSatPerKw),
		NextBatchClear:           time.Unix(int64(resp.NextBatchClearTimestamp), 0),
		AutoRenewExtensionBlocks: resp.AutoRenewExtensionBlocks,
	}, nil
}

// BatchSnapshot returns information about a target batch including the
// clearing price of the batch, and the set of orders matched within the batch.
//
// NOTE: This isn't wrapped in "native" types, as atm we only use this to
// shuffle information back to the client over our RPC interface.
func (c *Client) BatchSnapshot(ctx context.Context,
	targetBatch order.BatchID) (*auctioneerrpc.BatchSnapshotResponse,
	error) {

	return c.client.BatchSnapshot(ctx, &auctioneerrpc.BatchSnapshotRequest{
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
	req *auctioneerrpc.BatchSnapshotsRequest) (
	*auctioneerrpc.BatchSnapshotsResponse, error) {

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

	req := &auctioneerrpc.ServerNodeRatingRequest{
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

// MarketInfo returns information about the current number of open orders and
// open interest in units of all open markets.
//
// NOTE: This isn't wrapped in "native" types, as atm we only use this to
// shuffle information back to the client over our RPC interface.
func (c *Client) MarketInfo(ctx context.Context) (
	*auctioneerrpc.MarketInfoResponse, error) {

	return c.client.MarketInfo(ctx, &auctioneerrpc.MarketInfoRequest{})
}

// genSidecarAuth generates a set of valid authentication details to allow
// creating or deleting a hashmail mailbox.
func genSidecarAuth(sid [64]byte,
	ticket *sidecar.Ticket) (*auctioneerrpc.CipherBoxAuth, error) {

	strTicket, err := sidecar.EncodeToString(ticket)
	if err != nil {
		return nil, err
	}

	return &auctioneerrpc.CipherBoxAuth{
		Desc: &auctioneerrpc.CipherBoxDesc{
			StreamId: sid[:],
		},
		Auth: &auctioneerrpc.CipherBoxAuth_SidecarAuth{
			SidecarAuth: &auctioneerrpc.SidecarAuth{
				Ticket: strTicket,
			},
		},
	}, nil
}

// InitAccountCipherBox attempts to initialize a new CipherBox using the
// sidecar ticket as the authentication method.
func (c *Client) InitTicketCipherBox(ctx context.Context, sid [64]byte,
	ticket *sidecar.Ticket) error {

	streamAuth, err := genSidecarAuth(sid, ticket)
	if err != nil {
		return err
	}

	_, err = c.hashMailClient.NewCipherBox(ctx, streamAuth)
	return err
}

// DelSidecarMailbox tears down the mailbox the sidecar ticket recipient used
// to communicate with the provider.
func (c *Client) DelSidecarMailbox(ctx context.Context,
	streamID [64]byte, ticket *sidecar.Ticket) error {

	streamAuth, err := genSidecarAuth(streamID, ticket)
	if err != nil {
		return err
	}

	_, err = c.hashMailClient.DelCipherBox(ctx, streamAuth)
	return err
}

// genAcctAuth generates a valid authentication sig to allow a trader to delete
// or create a new hashmail mailbox.
func genAcctAuth(ctx context.Context, signer lndclient.SignerClient,
	sid [64]byte, acctKey *keychain.KeyDescriptor) (*auctioneerrpc.CipherBoxAuth, error) {

	streamSig, err := signer.SignMessage(
		ctx, sid[:], acctKey.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to sign cipher box "+
			"auth: %w", err)
	}

	acctKeyBytes := acctKey.PubKey.SerializeCompressed()
	return &auctioneerrpc.CipherBoxAuth{
		Desc: &auctioneerrpc.CipherBoxDesc{
			StreamId: sid[:],
		},
		Auth: &auctioneerrpc.CipherBoxAuth_AcctAuth{
			AcctAuth: &auctioneerrpc.PoolAccountAuth{
				AcctKey:   acctKeyBytes,
				StreamSig: streamSig,
			},
		},
	}, nil
}

// InitAccountCipherBox attempts to initialize a new CipherBox using the
// account key as an authentication mechanism.
func (c *Client) InitAccountCipherBox(ctx context.Context, sid [64]byte,
	acctKey *keychain.KeyDescriptor) error {

	streamAuth, err := genAcctAuth(ctx, c.cfg.Signer, sid, acctKey)
	if err != nil {
		return err
	}

	_, err = c.hashMailClient.NewCipherBox(ctx, streamAuth)
	return err
}

// DelAcctMailbox tears down the mailbox that the sidecar ticket provider used
// to communicate with the recipient.
func (c *Client) DelAcctMailbox(ctx context.Context, sid [64]byte,
	acctKey *keychain.KeyDescriptor) error {

	streamAuth, err := genAcctAuth(ctx, c.cfg.Signer, sid, acctKey)
	if err != nil {
		return err
	}

	_, err = c.hashMailClient.DelCipherBox(ctx, streamAuth)
	return err
}

// SendCipherBoxMsg attempts to the passed message into the cipher box
// identified by the passed stream ID. This message will be on-blocking as long
// as the buffer size of the stream is not exceed.
//
// TODO(roasbeef): option to expose a streaming interface?
func (c *Client) SendCipherBoxMsg(ctx context.Context, sid [64]byte,
	msg []byte) error {

	writeStream, err := c.hashMailClient.SendStream(ctx)
	if err != nil {
		return fmt.Errorf("unable to create send stream: %w", err)
	}

	err = writeStream.Send(&auctioneerrpc.CipherBox{
		Desc: &auctioneerrpc.CipherBoxDesc{
			StreamId: sid[:],
		},
		Msg: msg,
	})
	if err != nil {
		return err
	}

	return writeStream.CloseSend()
}

// RecvCipherBoxMsg attempts to read a message from the cipher box identified
// by the passed stream ID.
func (c *Client) RecvCipherBoxMsg(ctx context.Context,
	sid [64]byte) ([]byte, error) {

	streamDesc := &auctioneerrpc.CipherBoxDesc{
		StreamId: sid[:],
	}
	readStream, err := c.hashMailClient.RecvStream(ctx, streamDesc)
	if err != nil {
		return nil, fmt.Errorf("unable to create read stream: %w", err)
	}

	// TODO(roasbeef): need to cancel context?

	msg, err := readStream.Recv()
	if err != nil {
		return nil, err
	}

	return msg.Msg, nil
}

// MarshallNodeTier maps the node tier integer into the enum used on the RPC
// interface.
func MarshallNodeTier(nodeTier order.NodeTier) (auctioneerrpc.NodeTier, error) {
	switch nodeTier {
	case order.NodeTierDefault:
		return auctioneerrpc.NodeTier_TIER_DEFAULT, nil

	case order.NodeTier0:
		return auctioneerrpc.NodeTier_TIER_0, nil

	case order.NodeTier1:
		return auctioneerrpc.NodeTier_TIER_1, nil

	default:
		return 0, fmt.Errorf("unknown node tier: %v", nodeTier)
	}
}
