package pool

import (
	"context"
	"crypto/sha512"
	"strings"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/mailbox"
	"google.golang.org/grpc"
)

// getLnd returns an instance of the lnd services proxy.
func getLnd(network string, cfg *LndConfig) (*lndclient.GrpcLndServices,
	lnrpc.LightningClient, error) {

	conn, err := mailboxClientConnection(cfg)
	if err != nil {
		return nil, nil, err
	}

	services, err := lndclient.NewLndServicesFromAuthConn(
		lndclient.Network(network), conn, 0,
	)
	return services, lnrpc.NewLightningClient(conn), err
}

func mailboxClientConnection(cfg *LndConfig) (*grpc.ClientConn, error) {
	words := strings.Split(cfg.MailboxPassword, " ")
	var mnemonicWords [mailbox.NumPasswordWords]string
	copy(mnemonicWords[:], words)
	password := mailbox.PasswordMnemonicToEntropy(mnemonicWords)

	sid := sha512.Sum512(password[:])
	receiveSID := mailbox.GetSID(sid, true)
	sendSID := mailbox.GetSID(sid, false)

	privKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}
	ecdh := &keychain.PrivKeyECDH{PrivKey: privKey}

	transportConn := mailbox.NewClientConn(receiveSID, sendSID)
	noiseConn := mailbox.NewNoiseConn(ecdh, nil)

	dialOpts := []grpc.DialOption{
		grpc.WithContextDialer(transportConn.Dial),
		grpc.WithTransportCredentials(noiseConn),
		grpc.WithPerRPCCredentials(noiseConn),
	}

	ctx := context.Background()
	return grpc.DialContext(ctx, cfg.ConnectMailbox, dialOpts...)
}
