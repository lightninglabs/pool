package auctioneer

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/agora/client/order"
	"github.com/lightninglabs/loop/lndclient"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Client performs the client side part of auctions. This interface exists to be
// able to implement a stub.
type Client struct {
	client clmrpc.ChannelAuctioneerServerClient
	wallet lndclient.WalletKitClient
}

// NewClient returns a new instance to initiate auctions with.
func NewClient(serverAddress string, insecure bool, tlsPathServer string,
	wallet lndclient.WalletKitClient, dialOpts ...grpc.DialOption) (
	*Client, func(), error) {

	serverConn, err := getAuctionServerConn(
		serverAddress, insecure, tlsPathServer, dialOpts...,
	)
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		_ = serverConn.Close()
	}

	return &Client{
		client: clmrpc.NewChannelAuctioneerServerClient(serverConn),
		wallet: wallet,
	}, cleanup, nil
}

// getAuctionServerConn returns a connection to the auction server.
func getAuctionServerConn(address string, insecure bool, tlsPath string,
	dialOpts ...grpc.DialOption) (*grpc.ClientConn, error) {

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

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
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

// CloseAccount sends an intent to the auctioneer that we'd like to close the
// account with the associated trader key by withdrawing the funds to the given
// outputs. The auctioneer's signature is returned, allowing us to broadcast a
// transaction sweeping the account.
func (c *Client) CloseAccount(ctx context.Context, traderKey *btcec.PublicKey,
	outputs []*wire.TxOut) ([]byte, error) {

	var rpcOutputs []*clmrpc.Output
	if len(outputs) > 0 {
		rpcOutputs = make([]*clmrpc.Output, 0, len(outputs))
		for _, output := range outputs {
			rpcOutputs = append(rpcOutputs, &clmrpc.Output{
				Value:  uint32(output.Value),
				Script: output.PkScript,
			})
		}
	}

	resp, err := c.client.ModifyAccount(ctx, &clmrpc.ServerModifyAccountRequest{
		UserSubKey: traderKey.SerializeCompressed(),
		NewOutputs: rpcOutputs,
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
		UserSubKey:     o.Details().AcctKey.SerializeCompressed(),
		RateFixed:      int64(o.Details().FixedRate),
		Amt:            int64(o.Details().Amt),
		OrderNonce:     nonce[:],
		OrderSig:       serverParams.RawSig,
		MultiSigKey:    serverParams.MultiSigKey[:],
		NodePub:        serverParams.NodePubkey[:],
		NodeAddr:       nodeAddrs,
		FundingFeeRate: int64(o.Details().FundingFeeRate),
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
	case clmrpc.ServerOrderStateResponse_SUBMITTED:
		return order.StateSubmitted, resp.UnitsUnfulfilled, nil

	case clmrpc.ServerOrderStateResponse_CLEARED:
		return order.StateCleared, resp.UnitsUnfulfilled, nil

	case clmrpc.ServerOrderStateResponse_PARTIAL_FILL:
		return order.StatePartialFill, resp.UnitsUnfulfilled, nil

	case clmrpc.ServerOrderStateResponse_EXECUTED:
		return order.StateExecuted, resp.UnitsUnfulfilled, nil

	case clmrpc.ServerOrderStateResponse_CANCELED:
		return order.StateCanceled, resp.UnitsUnfulfilled, nil

	case clmrpc.ServerOrderStateResponse_EXPIRED:
		return order.StateExpired, resp.UnitsUnfulfilled, nil

	case clmrpc.ServerOrderStateResponse_FAILED:
		return order.StateFailed, resp.UnitsUnfulfilled, nil

	default:
		return 0, 0, fmt.Errorf("invalid order state: %v", resp.State)
	}
}
