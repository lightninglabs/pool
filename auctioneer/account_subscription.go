package auctioneer

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/agora/client/order"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
)

// acctSubscription holds the account that is subscribed to updates from the
// auction. It can also perform the 3-way authentication handshake that is
// needed to authenticate a trader for a subscription.
type acctSubscription struct {
	acctKey       *keychain.KeyDescriptor
	commitHash    [32]byte
	sendMsg       func(*clmrpc.ClientAuctionMessage) error
	signer        lndclient.SignerClient
	challengeChan chan [32]byte
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
	err = s.sendMsg(&clmrpc.ClientAuctionMessage{
		Msg: &clmrpc.ClientAuctionMessage_Commit{
			Commit: &clmrpc.AccountCommitment{
				CommitHash:   s.commitHash[:],
				BatchVersion: uint32(order.CurrentVersion),
			},
		},
	})
	if err != nil {
		return err
	}

	// We can't sign anything if we haven't received the server's challenge
	// yet. So we'll wait for the message to arrive.
	select {
	case serverChallenge, more := <-s.challengeChan:
		if !more {
			return fmt.Errorf("channel closed before challenge " +
				"was received")
		}

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
		return s.sendMsg(&clmrpc.ClientAuctionMessage{
			Msg: &clmrpc.ClientAuctionMessage_Subscribe{
				Subscribe: &clmrpc.AccountSubscription{
					UserSubKey:  acctPubKey[:],
					CommitNonce: nonce[:],
					AuthSig:     sig,
				},
			},
		})

	case <-ctx.Done():
		return fmt.Errorf("context canceled before challenge was " +
			"received")
	}
}
