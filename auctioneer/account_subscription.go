package auctioneer

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightningnetwork/lnd/keychain"
)

// acctSubscription holds the account that is subscribed to updates from the
// auction. It can also perform the 3-way authentication handshake that is
// needed to authenticate a trader for a subscription.
type acctSubscription struct {
	acctKey    *keychain.KeyDescriptor
	commitHash [32]byte
	sendMsg    func(*poolrpc.ClientAuctionMessage) error
	signer     lndclient.SignerClient
	msgChan    chan *poolrpc.ServerAuctionMessage
	errChan    chan error
	quit       chan struct{}
}

// authenticate performs the 3-way authentication handshake between the trader
// and the auctioneer. This method blocks until the handshake is complete or
// fails which involves 1.5 round trips to the server.
func (s *acctSubscription) authenticate(ctx context.Context) error {
	// Create the account commitment hash by combining a random nonce with
	// the account's sub key.
	var (
		nonce      [32]byte
		acctPubKey [33]byte
	)
	_, err := rand.Read(nonce[:])
	if err != nil {
		return err
	}
	copy(acctPubKey[:], s.acctKey.PubKey.SerializeCompressed())
	s.commitHash = account.CommitAccount(acctPubKey, nonce)

	// Now send the commitment to the server which should trigger it to send
	// a challenge back. We need to track the subscription from now on so
	// the goroutine reading the incoming messages knows which subscription
	// to add the received challenge to.
	err = s.sendMsg(&poolrpc.ClientAuctionMessage{
		Msg: &poolrpc.ClientAuctionMessage_Commit{
			Commit: &poolrpc.AccountCommitment{
				CommitHash:   s.commitHash[:],
				BatchVersion: uint32(order.CurrentVersion),
			},
		},
	})
	if err != nil {
		return err
	}

	// We can't sign anything if we haven't received the server's challenge
	// yet. So we'll wait for the message or an error to arrive.
	select {
	case srvMsg, more := <-s.msgChan:
		if !more {
			return fmt.Errorf("channel closed before challenge " +
				"was received")
		}

		msg, ok := srvMsg.Msg.(*poolrpc.ServerAuctionMessage_Challenge)
		if !ok {
			return fmt.Errorf("unexpected server message in auth "+
				"process: %v", msg)
		}
		var serverChallenge [32]byte
		copy(serverChallenge[:], msg.Challenge.Challenge)

		// Finally sign the challenge to authenticate ourselves. We now
		// reveal the nonce we used for the commitment so the server can
		// verify the information.
		authHash := account.AuthHash(s.commitHash, serverChallenge)
		sig, err := s.signer.SignMessage(
			ctx, authHash[:], s.acctKey.KeyLocator,
		)
		if err != nil {
			return err
		}
		return s.sendMsg(&poolrpc.ClientAuctionMessage{
			Msg: &poolrpc.ClientAuctionMessage_Subscribe{
				Subscribe: &poolrpc.AccountSubscription{
					TraderKey:   acctPubKey[:],
					CommitNonce: nonce[:],
					AuthSig:     sig,
				},
			},
		})

	case err := <-s.errChan:
		return fmt.Errorf("error during authentication, before "+
			"sending subscribe: %v", err)

	case <-ctx.Done():
		return fmt.Errorf("context canceled before challenge was " +
			"received")

	case <-s.quit:
		return ErrAuthCanceled
	}
}
