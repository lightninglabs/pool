package auctioneer

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightninglabs/pool/internal/test"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	testTraderKeyStr = "036b51e0cc2d9e5988ee4967e0ba67ef3727bb633fea21" +
		"a0af58e0c9395446ba09"
	testRawTraderKey, _ = hex.DecodeString(testTraderKeyStr)
	testTraderKey, _    = btcec.ParsePubKey(testRawTraderKey, btcec.S256())
	testAccountDesc     = &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{},
		PubKey:     testTraderKey,
	}
	testSigner     = test.NewMockSigner()
	defaultTimeout = 100 * time.Millisecond
)

// TestAccountSubscriptionAuthenticate tests that the 3-way authentication
// handshake is performed correctly when subscribing for account updates.
func TestAccountSubscriptionAuthenticate(t *testing.T) {
	var (
		msgChan    = make(chan *poolrpc.ClientAuctionMessage)
		srvMsgChan = make(chan *poolrpc.ServerAuctionMessage)
		errChan    = make(chan error)
		sendMsg    = func(msg *poolrpc.ClientAuctionMessage) error {
			msgChan <- msg
			return nil
		}
		sub = &acctSubscription{
			acctKey: testAccountDesc,
			sendMsg: sendMsg,
			signer:  testSigner,
			msgChan: srvMsgChan,
		}
	)

	// First, kick off the auth handshake in a goroutine. Every step will
	// block because we don't use buffered channels.
	go func() {
		errChan <- sub.authenticate(context.Background())
	}()

	// Step 1: We expect a commitment message.
	select {
	case msg := <-msgChan:
		if _, ok := msg.Msg.(*poolrpc.ClientAuctionMessage_Commit); !ok {
			t.Fatalf("unexpected message type: %v", msg)
		}

	case <-time.After(defaultTimeout):
		t.Fatalf("did not receive commit message before timeout")
	}

	// Step 2: Simulate the server sending back the challenge.
	srvMsgChan <- &poolrpc.ServerAuctionMessage{
		Msg: &poolrpc.ServerAuctionMessage_Challenge{
			Challenge: &poolrpc.ServerChallenge{
				Challenge: []byte{11, 99, 11},
			},
		},
	}

	// Step 3: We expect the final message, the subscription.
	select {
	case msg := <-msgChan:
		subMsg, ok := msg.Msg.(*poolrpc.ClientAuctionMessage_Subscribe)
		if !ok {
			t.Fatalf("unexpected message type: %v", msg)
		}
		if !bytes.Equal(subMsg.Subscribe.AuthSig, testSigner.Signature) {
			t.Fatalf("unexpected signature. got %x, wanted %x",
				subMsg.Subscribe.AuthSig, testSigner.Signature)
		}

	case <-time.After(defaultTimeout):
		t.Fatalf("did not receive commit message before timeout")
	}
}

// TestAccountSubscriptionAuthenticateAbort tests that the 3-way authentication
// handshake is canceled if the channel is closed prematurely.
func TestAccountSubscriptionAuthenticateAbort(t *testing.T) {
	var (
		msgChan    = make(chan *poolrpc.ClientAuctionMessage)
		srvMsgChan = make(chan *poolrpc.ServerAuctionMessage)
		errChan    = make(chan error)
		sendMsg    = func(msg *poolrpc.ClientAuctionMessage) error {
			msgChan <- msg
			return nil
		}
		sub = &acctSubscription{
			acctKey: testAccountDesc,
			sendMsg: sendMsg,
			signer:  testSigner,
			msgChan: srvMsgChan,
		}
	)

	// First, kick off the auth handshake in a goroutine. Every step will
	// block because we don't use buffered channels.
	go func() {
		errChan <- sub.authenticate(context.Background())
	}()

	// Step 1: We expect a commitment message.
	select {
	case msg := <-msgChan:
		if _, ok := msg.Msg.(*poolrpc.ClientAuctionMessage_Commit); !ok {
			t.Fatalf("unexpected message type: %v", msg)
		}

	case <-time.After(defaultTimeout):
		t.Fatalf("did not receive commit message before timeout")
	}

	// Step 2: Simulate the trader shutting down instead of receiving the
	// challenge.
	close(srvMsgChan)

	// There should be an error in the chan now.
	select {
	case err := <-errChan:
		if !strings.Contains(err.Error(), "channel closed") {
			t.Fatalf("unexpected error: %v", err)
		}

	case <-time.After(defaultTimeout):
		t.Fatalf("did not receive commit message before timeout")
	}
}

// TestAccountSubscriptionAuthenticateContextClose tests that the 3-way
// authentication handshake is canceled if the context is canceled prematurely.
func TestAccountSubscriptionAuthenticateContextClose(t *testing.T) {
	var (
		msgChan    = make(chan *poolrpc.ClientAuctionMessage)
		srvMsgChan = make(chan *poolrpc.ServerAuctionMessage)
		errChan    = make(chan error)
		sendMsg    = func(msg *poolrpc.ClientAuctionMessage) error {
			msgChan <- msg
			return nil
		}
		sub = &acctSubscription{
			acctKey: testAccountDesc,
			sendMsg: sendMsg,
			signer:  testSigner,
			msgChan: srvMsgChan,
		}
		ctxc, cancel = context.WithCancel(context.Background())
	)

	// First, kick off the auth handshake in a goroutine. Every step will
	// block because we don't use buffered channels.
	go func() {
		errChan <- sub.authenticate(ctxc)
	}()

	// Step 1: We expect a commitment message.
	select {
	case msg := <-msgChan:
		if _, ok := msg.Msg.(*poolrpc.ClientAuctionMessage_Commit); !ok {
			t.Fatalf("unexpected message type: %v", msg)
		}

	case <-time.After(defaultTimeout):
		t.Fatalf("did not receive commit message before timeout")
	}

	// Step 2: Simulate the trader shutting down instead of receiving the
	// challenge.
	cancel()

	// There should be an error in the chan now.
	select {
	case err := <-errChan:
		if !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("unexpected error: %v", err)
		}

	case <-time.After(defaultTimeout):
		t.Fatalf("did not receive commit message before timeout")
	}
}
