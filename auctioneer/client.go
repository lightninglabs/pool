package auctioneer

import (
	"crypto/tls"
	"fmt"

	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/loop/lndclient"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Client performs the client side part of auctions. This interface exists to be
// able to implement a stub.
type Client struct {
	clmrpc.ChannelAuctioneerServerClient
}

// NewClient returns a new instance to initiate auctions with.
func NewClient(dbDir string, serverAddress string, insecure bool,
	tlsPathServer string, lnd *lndclient.LndServices) (*Client, func(),
	error) {

	serverConn, err := getAuctionServerConn(
		serverAddress, insecure, tlsPathServer,
	)
	if err != nil {
		return nil, nil, err
	}

	server := clmrpc.NewChannelAuctioneerServerClient(serverConn)

	client := &Client{
		ChannelAuctioneerServerClient: server,
	}

	cleanup := func() {
		serverConn.Close()
	}

	return client, cleanup, nil
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
