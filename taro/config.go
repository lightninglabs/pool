package taro

import (
	"fmt"
	"os"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

var (
	// maxMsgRecvSize is the largest message our client will receive. We
	// set this to 200MiB atm.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(lnrpc.MaxGrpcMsgSize)
)

type Config struct {
	Disable bool `long:"disable" description:"disable taro daemon interaction"`

	Host string `long:"host" description:"taro instance rpc address"`

	// MacaroonPath is the path to tarod's admin.macaroon file.
	MacaroonPath string `long:"macaroonpath" description:"The full path to tarod's admin.macaroon file"`

	TLSPath string `long:"tlspath" description:"Path to tarod TLS certificate"`
}

// ClientConn returns a client connection to the Taro instance specified in the
// configuration.
func (c *Config) ClientConn() (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
	}

	// Load the specified TLS certificate and build transport credentials
	creds, err := credentials.NewClientTLSFromFile(c.TLSPath, "")
	if err != nil {
		return nil, err
	}
	opts = append(opts, grpc.WithTransportCredentials(creds))

	// Now we append the macaroon credentials to the dial options.
	macBytes, err := os.ReadFile(c.MacaroonPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read macaroon at path %s: "+
			": %w", c.MacaroonPath, err)
	}
	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("unable to decode macaroon: %w", err)
	}

	cred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, fmt.Errorf("error cloning mac: %w", err)
	}
	opts = append(opts, grpc.WithPerRPCCredentials(cred))

	conn, err := grpc.Dial(c.Host, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %w",
			err)
	}

	return conn, nil
}
