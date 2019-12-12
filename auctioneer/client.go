package auctioneer

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/clmrpc"
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
	wallet lndclient.WalletKitClient) (*Client, func(), error) {

	serverConn, err := getAuctionServerConn(
		serverAddress, insecure, tlsPathServer,
	)
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		serverConn.Close()
	}

	return &Client{
		client: clmrpc.NewChannelAuctioneerServerClient(serverConn),
		wallet: wallet,
	}, cleanup, nil
}

// getAuctionServerConn returns a connection to the auction server.
func getAuctionServerConn(address string, insecure bool, tlsPath string) (
	*grpc.ClientConn, error) {

	// Create a dial options array.
	opts := []grpc.DialOption{}

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

// ReserveAccount reserves an account with the auctioneer. It returns a the
// public key we should use for them in our 2-of-2 multi-sig construction.
func (c *Client) ReserveAccount(ctx context.Context,
	traderKey [33]byte) ([33]byte, error) {

	resp, err := c.client.ReserveAccount(ctx, &clmrpc.ReserveAccountRequest{
		UserSubKey: traderKey[:],
	})
	if err != nil {
		return [33]byte{}, err
	}

	var auctioneerKey [33]byte
	copy(auctioneerKey[:], resp.AuctioneerKey)
	return auctioneerKey, nil
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
		UserSubKey:    account.TraderKey[:],
	})
	return err
}
