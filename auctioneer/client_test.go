package auctioneer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/order"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestJitterBackoffBounds samples the jitter helper for a typical configured
// backoff and asserts results fall in the expected [backoff, backoff +
// backoff/4] range and aren't pinned to a single value.
func TestJitterBackoffBounds(t *testing.T) {
	t.Parallel()

	const (
		base    = 5 * time.Second
		samples = 200
	)
	seen := make(map[time.Duration]struct{}, samples)
	for i := 0; i < samples; i++ {
		got := jitterBackoff(base)
		if got < base || got > base+base/4 {
			t.Fatalf("jitterBackoff(%v) = %v, out of [%v, %v]",
				base, got, base, base+base/4)
		}
		seen[got] = struct{}{}
	}

	// In 200 samples over a 1.25s window of nanosecond resolution we
	// expect many distinct values. If we get only a handful, jitter is
	// broken.
	if len(seen) < 10 {
		t.Fatalf("expected diverse jitter samples, "+
			"only got %d unique values", len(seen))
	}
}

// fakeServerStream is a minimal implementation of
// ChannelAuctioneer_SubscribeBatchAuctionClient that returns predetermined
// results from Recv and captures client-sent messages on `sent` (when
// non-nil). It is only sufficient for driving the client's read loop and
// the auth handshake.
type fakeServerStream struct {
	grpc.ClientStream

	recv chan recvResult
	sent chan *auctioneerrpc.ClientAuctionMessage
}

type recvResult struct {
	msg *auctioneerrpc.ServerAuctionMessage
	err error
}

func (s *fakeServerStream) Send(msg *auctioneerrpc.ClientAuctionMessage) error {
	if s.sent != nil {
		s.sent <- msg
	}
	return nil
}

func (s *fakeServerStream) Recv() (*auctioneerrpc.ServerAuctionMessage, error) {
	r := <-s.recv
	return r.msg, r.err
}

func (s *fakeServerStream) CloseSend() error {
	return nil
}

// fakeAuctioneerClient embeds the real ChannelAuctioneerClient interface so it
// satisfies all 14 methods by nil-deref (none are called in this test other
// than the two we override below).
type fakeAuctioneerClient struct {
	auctioneerrpc.ChannelAuctioneerClient

	stream auctioneerrpc.ChannelAuctioneer_SubscribeBatchAuctionClient
}

func (f *fakeAuctioneerClient) Terms(ctx context.Context,
	in *auctioneerrpc.TermsRequest,
	opts ...grpc.CallOption) (*auctioneerrpc.TermsResponse, error) {

	return &auctioneerrpc.TermsResponse{}, nil
}

func (f *fakeAuctioneerClient) SubscribeBatchAuction(ctx context.Context,
	opts ...grpc.CallOption) (
	auctioneerrpc.ChannelAuctioneer_SubscribeBatchAuctionClient, error) {

	return f.stream, nil
}

// noPendingBatchSource is a BatchSource stub that always reports "no pending
// batch", letting checkPendingBatch return cleanly.
type noPendingBatchSource struct{}

func (noPendingBatchSource) PendingBatchSnapshot() (
	*clientdb.LocalBatchSnapshot, error) {

	return nil, account.ErrNoPendingBatch
}

// newTestClient returns a Client wired up just enough to drive
// readIncomingStream against a fake server stream.
func newTestClient(stream auctioneerrpc.ChannelAuctioneer_SubscribeBatchAuctionClient,
) (*Client, chan error) {

	mainErrChan := make(chan error, 1)
	c := &Client{
		serverStream:    stream,
		FromServerChan:  make(chan *auctioneerrpc.ServerAuctionMessage),
		StreamErrChan:   mainErrChan,
		errChanSwitch:   NewErrChanSwitch(mainErrChan),
		quit:            make(chan struct{}),
		subscribedAccts: make(map[[33]byte]*acctSubscription),
	}
	c.errChanSwitch.Start()
	return c, mainErrChan
}

// runReadLoop runs readIncomingStream in a goroutine and returns a channel
// that closes when the loop exits.
func runReadLoop(c *Client) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		c.readIncomingStream()
		close(done)
	}()
	return done
}

// TestReadIncomingStreamEOFTriggersReconnect ensures that an io.EOF received
// on the server stream is surfaced as ErrServerErrored on the error channel,
// which is the signal the rpcserver consumer uses to trigger reconnect logic.
//
// This is a regression test: EOF was previously reported as a separate
// "ErrServerShutdown" sentinel that the consumer silently ignored under the
// (incorrect) assumption that the client had already scheduled its own
// reconnect. The result was a permanently dead subscription stream after any
// clean close (proxy/LB timeout, planned server shutdown, etc.), with the
// trader being filtered as offline until the process restarted.
func TestReadIncomingStreamEOFTriggersReconnect(t *testing.T) {
	t.Parallel()

	stream := &fakeServerStream{recv: make(chan recvResult, 1)}
	c, mainErrChan := newTestClient(stream)
	defer c.errChanSwitch.Stop()
	defer close(c.quit)

	// Tell the fake stream to return io.EOF, simulating the server (or an
	// intermediate proxy) cleanly closing its side of the bidi stream.
	stream.recv <- recvResult{err: io.EOF}

	done := runReadLoop(c)

	select {
	case err := <-mainErrChan:
		if !errors.Is(err, ErrServerErrored) {
			t.Fatalf("expected ErrServerErrored on EOF, got: %v",
				err)
		}
	case <-time.After(defaultTimeout):
		t.Fatal("timed out waiting for error after EOF")
	}

	select {
	case <-done:
	case <-time.After(defaultTimeout):
		t.Fatal("readIncomingStream did not return after EOF")
	}
}

// TestReadIncomingStreamTransportErrorTriggersReconnect ensures non-EOF
// transport errors continue to be surfaced as ErrServerErrored. This is the
// pre-existing behaviour we want to preserve after unifying it with the EOF
// path.
func TestReadIncomingStreamTransportErrorTriggersReconnect(t *testing.T) {
	t.Parallel()

	stream := &fakeServerStream{recv: make(chan recvResult, 1)}
	c, mainErrChan := newTestClient(stream)
	defer c.errChanSwitch.Stop()
	defer close(c.quit)

	// A "transport is closing" style error, which is what gRPC surfaces
	// when the underlying TCP connection breaks abruptly.
	stream.recv <- recvResult{
		err: status.Error(codes.Unavailable, "transport is closing"),
	}

	done := runReadLoop(c)

	select {
	case err := <-mainErrChan:
		if !errors.Is(err, ErrServerErrored) {
			t.Fatalf("expected ErrServerErrored on transport "+
				"error, got: %v", err)
		}
	case <-time.After(defaultTimeout):
		t.Fatal("timed out waiting for error after transport failure")
	}

	select {
	case <-done:
	case <-time.After(defaultTimeout):
		t.Fatal("readIncomingStream did not return after transport " +
			"failure")
	}
}

// TestReadIncomingStreamContextCanceledDoesNotReconnect ensures that a
// codes.Canceled error (which happens when *we* cancel the stream context
// during shutdown or a planned reconnect) does NOT surface an error to the
// consumer, so we don't accidentally schedule a second reconnect.
func TestReadIncomingStreamContextCanceledDoesNotReconnect(t *testing.T) {
	t.Parallel()

	stream := &fakeServerStream{recv: make(chan recvResult, 1)}
	c, mainErrChan := newTestClient(stream)
	defer c.errChanSwitch.Stop()
	defer close(c.quit)

	stream.recv <- recvResult{
		err: status.Error(codes.Canceled, "context canceled"),
	}

	done := runReadLoop(c)

	select {
	case <-done:
	case <-time.After(defaultTimeout):
		t.Fatal("readIncomingStream did not return after cancel")
	}

	select {
	case err := <-mainErrChan:
		t.Fatalf("unexpected error surfaced on cancel: %v", err)
	case <-time.After(defaultTimeout):
		// Expected: no error surfaced.
	}
}

// TestConnectAndAuthenticateCleansUpOnError drives a full
// connectAndAuthenticate call in recovery mode against a scripted fake stream
// for each per-account error path that the auctioneer can return after the
// Subscribe message, and asserts the subscription entry is always removed
// from c.subscribedAccts on return.
//
// Regression: previously the entry was added to the map before authenticate
// ran (so readIncomingStream could route the server's Challenge/Error back
// to it) and was never removed on error paths. A later StartAccountSubscription
// for the same account — typically when handleStateOpen runs after on-chain
// confirmation — would hit the "already subscribed" early-return guard at the
// top of connectAndAuthenticate and silently no-op without sending a fresh
// Commit. The per-account 3-way handshake never ran and the trader stayed
// filtered as offline at matching time until the process restarted.
func TestConnectAndAuthenticateCleansUpOnError(t *testing.T) {
	t.Parallel()

	var pubKey [33]byte
	copy(pubKey[:], testAccountDesc.PubKey.SerializeCompressed())

	cases := []struct {
		name     string
		errResp  *auctioneerrpc.SubscribeError
		checkRes func(t *testing.T, sub *acctSubscription,
			canRecover bool, err error)
	}{
		{
			// Realistic case: RecoverAccounts probes a key the
			// auctioneer hasn't yet seen on chain.
			name: "account does not exist",
			errResp: &auctioneerrpc.SubscribeError{
				ErrorCode: auctioneerrpc.SubscribeError_ACCOUNT_DOES_NOT_EXIST,
				TraderKey: pubKey[:],
			},
			checkRes: func(t *testing.T, sub *acctSubscription,
				canRecover bool, err error) {

				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if canRecover {
					t.Fatal("expected canRecover=false")
				}
				if sub == nil {
					t.Fatal("expected non-nil subscription")
				}
			},
		},
		{
			// The auctioneer knows about a reservation for this
			// key but the funding tx hasn't confirmed yet. The
			// function returns a non-nil sub *and* a typed error,
			// which makes the cleanup invariant especially easy
			// to get wrong.
			name: "incomplete account reservation",
			errResp: &auctioneerrpc.SubscribeError{
				ErrorCode: auctioneerrpc.SubscribeError_INCOMPLETE_ACCOUNT_RESERVATION,
				TraderKey: pubKey[:],
				AccountReservation: &auctioneerrpc.AuctionAccount{
					Value:         100_000,
					Expiry:        144,
					TraderKey:     pubKey[:],
					AuctioneerKey: bytes.Repeat([]byte{0x02}, 33),
					BatchKey:      bytes.Repeat([]byte{0x03}, 33),
					HeightHint:    1,
				},
			},
			checkRes: func(t *testing.T, sub *acctSubscription,
				canRecover bool, err error) {

				var resErr *AcctResNotCompletedError
				if !errors.As(err, &resErr) {
					t.Fatalf("expected "+
						"AcctResNotCompletedError, "+
						"got %v", err)
				}
				if !canRecover {
					t.Fatal("expected canRecover=true")
				}
				if sub == nil {
					t.Fatal("expected non-nil subscription")
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runCleanupCase(t, pubKey, tc.errResp, tc.checkRes)
		})
	}
}

// runCleanupCase wires up a fresh Client + fake stream, drives a full
// connectAndAuthenticate handshake in recovery mode, feeds the supplied
// error response back at the Subscribe step, runs the caller's assertions on
// the return values, and finally asserts that subscribedAccts is empty.
func runCleanupCase(t *testing.T, pubKey [33]byte,
	errResp *auctioneerrpc.SubscribeError,
	checkRes func(t *testing.T, sub *acctSubscription, canRecover bool,
		err error)) {

	stream := &fakeServerStream{
		recv: make(chan recvResult, 1),
		sent: make(chan *auctioneerrpc.ClientAuctionMessage, 2),
	}

	mainErrChan := make(chan error, 1)
	c := &Client{
		cfg: &Config{
			Signer:       testSigner,
			BatchVersion: order.LatestBatchVersion,
		},
		serverStream:    stream,
		FromServerChan:  make(chan *auctioneerrpc.ServerAuctionMessage),
		StreamErrChan:   mainErrChan,
		errChanSwitch:   NewErrChanSwitch(mainErrChan),
		quit:            make(chan struct{}),
		subscribedAccts: make(map[[33]byte]*acctSubscription),
	}
	c.errChanSwitch.Start()
	defer c.errChanSwitch.Stop()
	defer close(c.quit)

	// Run the read loop in the background so server responses are routed
	// to the subscription's msgChan via subscribedAccts lookups.
	readDone := make(chan struct{})
	go func() {
		c.readIncomingStream()
		close(readDone)
	}()

	type result struct {
		sub        *acctSubscription
		canRecover bool
		err        error
	}
	resCh := make(chan result, 1)
	go func() {
		sub, canRecover, err := c.connectAndAuthenticate(
			context.Background(), testAccountDesc, true,
		)
		resCh <- result{sub, canRecover, err}
	}()

	// Step 1: capture the Commit and echo its commitHash back in the
	// Challenge so readIncomingStream can route it to the right sub.
	var commitHash []byte
	select {
	case msg := <-stream.sent:
		commit, ok := msg.Msg.(*auctioneerrpc.ClientAuctionMessage_Commit)
		if !ok {
			t.Fatalf("expected Commit, got %T", msg.Msg)
		}
		commitHash = commit.Commit.CommitHash
	case <-time.After(defaultTimeout):
		t.Fatal("did not receive Commit from client")
	}

	// Step 2: feed back the Challenge.
	stream.recv <- recvResult{
		msg: &auctioneerrpc.ServerAuctionMessage{
			Msg: &auctioneerrpc.ServerAuctionMessage_Challenge{
				Challenge: &auctioneerrpc.ServerChallenge{
					Challenge:  []byte{1, 2, 3, 4},
					CommitHash: commitHash,
				},
			},
		},
	}

	// Step 3: drain the Subscribe message so authenticate() returns.
	select {
	case msg := <-stream.sent:
		if _, ok := msg.Msg.(*auctioneerrpc.ClientAuctionMessage_Subscribe); !ok {
			t.Fatalf("expected Subscribe, got %T", msg.Msg)
		}
	case <-time.After(defaultTimeout):
		t.Fatal("did not receive Subscribe from client")
	}

	// Step 4: server responds with the supplied error.
	stream.recv <- recvResult{
		msg: &auctioneerrpc.ServerAuctionMessage{
			Msg: &auctioneerrpc.ServerAuctionMessage_Error{
				Error: errResp,
			},
		},
	}

	// Step 5: connectAndAuthenticate should return; let the caller assert
	// the return values.
	var res result
	select {
	case res = <-resCh:
	case <-time.After(defaultTimeout):
		t.Fatal("connectAndAuthenticate did not return")
	}
	checkRes(t, res.sub, res.canRecover, res.err)

	// In every error case, the subscription must NOT be left in the map.
	// A later StartAccountSubscription for this account would otherwise
	// hit the "already subscribed" guard and silently no-op without ever
	// sending a fresh Commit.
	c.subscribedAcctsMtx.Lock()
	_, present := c.subscribedAccts[pubKey]
	c.subscribedAcctsMtx.Unlock()
	if present {
		t.Fatal("subscribedAccts entry was not cleaned up; later " +
			"subscribes for the same account would silently no-op")
	}

	// Clean up the background read loop. Sending io.EOF unblocks the
	// Recv call and lets readIncomingStream exit cleanly.
	stream.recv <- recvResult{err: io.EOF}
	select {
	case <-readDone:
	case <-time.After(defaultTimeout):
		t.Fatal("read loop did not exit after EOF")
	}
}

// TestHandleServerShutdownPartialResubscribeFailure asserts that
// HandleServerShutdown attempts to re-subscribe every account even when one
// of the handshakes fails server-side. The current loop bails on the first
// error, silently leaving the remaining accounts un-subscribed.
func TestHandleServerShutdownPartialResubscribeFailure(t *testing.T) {
	t.Parallel()

	// Three distinct account keys.
	keys := make([]*keychain.KeyDescriptor, 3)
	for i := range keys {
		priv, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatalf("could not generate key: %v", err)
		}
		keys[i] = &keychain.KeyDescriptor{PubKey: priv.PubKey()}
	}
	keyBytes := func(k *keychain.KeyDescriptor) [33]byte {
		var b [33]byte
		copy(b[:], k.PubKey.SerializeCompressed())
		return b
	}

	// Stream that connectServerStream will hand back after closeStream.
	stream := &fakeServerStream{
		recv: make(chan recvResult, 8),
		sent: make(chan *auctioneerrpc.ClientAuctionMessage, 8),
	}

	mainErrChan := make(chan error, 4)
	c := &Client{
		cfg: &Config{
			Signer:       testSigner,
			BatchVersion: order.LatestBatchVersion,
			MinBackoff:   time.Millisecond,
			MaxBackoff:   time.Millisecond,
			BatchSource:  noPendingBatchSource{},
		},
		client:          &fakeAuctioneerClient{stream: stream},
		FromServerChan:  make(chan *auctioneerrpc.ServerAuctionMessage),
		StreamErrChan:   mainErrChan,
		errChanSwitch:   NewErrChanSwitch(mainErrChan),
		quit:            make(chan struct{}),
		subscribedAccts: make(map[[33]byte]*acctSubscription),
	}
	c.errChanSwitch.Start()
	defer c.errChanSwitch.Stop()
	defer close(c.quit)

	// Pre-populate the subscribed accounts. HandleServerShutdown only reads
	// acctKey out of each subscription to seed its re-subscribe loop; the
	// channels here are placeholders.
	for _, k := range keys {
		c.subscribedAccts[keyBytes(k)] = &acctSubscription{
			acctKey: k,
			msgChan: make(chan *auctioneerrpc.ServerAuctionMessage),
			quit:    make(chan struct{}),
		}
	}

	// Orchestrator: walk each handshake through Challenge + final
	// response. The first attempt always gets ACCOUNT_DOES_NOT_EXIST;
	// the rest get Success. Failing on the first attempt (rather than a
	// fixed key) keeps the assertion deterministic under Go's randomized
	// map iteration order.
	var (
		attemptedMtx sync.Mutex
		attempted    = make(map[[33]byte]struct{})
	)
	go func() {
		first := true
		for {
			var msg *auctioneerrpc.ClientAuctionMessage
			select {
			case msg = <-stream.sent:
			case <-c.quit:
				return
			}
			commit, ok := msg.Msg.(*auctioneerrpc.ClientAuctionMessage_Commit)
			if !ok {
				continue
			}

			// Send Challenge back with the matching commitHash so
			// readIncomingStream can route it.
			stream.recv <- recvResult{
				msg: &auctioneerrpc.ServerAuctionMessage{
					Msg: &auctioneerrpc.ServerAuctionMessage_Challenge{
						Challenge: &auctioneerrpc.ServerChallenge{
							Challenge:  []byte{1, 2, 3, 4},
							CommitHash: commit.Commit.CommitHash,
						},
					},
				},
			}

			// Wait for the Subscribe with the trader key.
			var subMsg *auctioneerrpc.ClientAuctionMessage
			select {
			case subMsg = <-stream.sent:
			case <-c.quit:
				return
			}
			sub, ok := subMsg.Msg.(*auctioneerrpc.ClientAuctionMessage_Subscribe)
			if !ok {
				continue
			}

			var traderKey [33]byte
			copy(traderKey[:], sub.Subscribe.TraderKey)
			attemptedMtx.Lock()
			attempted[traderKey] = struct{}{}
			attemptedMtx.Unlock()

			final := &auctioneerrpc.ServerAuctionMessage{
				Msg: &auctioneerrpc.ServerAuctionMessage_Success{
					Success: &auctioneerrpc.SubscribeSuccess{
						TraderKey: sub.Subscribe.TraderKey,
					},
				},
			}
			if first {
				final = &auctioneerrpc.ServerAuctionMessage{
					Msg: &auctioneerrpc.ServerAuctionMessage_Error{
						Error: &auctioneerrpc.SubscribeError{
							ErrorCode: auctioneerrpc.SubscribeError_ACCOUNT_DOES_NOT_EXIST,
							TraderKey: sub.Subscribe.TraderKey,
						},
					},
				}
				first = false
			}
			stream.recv <- recvResult{msg: final}
		}
	}()

	// Drive HandleServerShutdown in a goroutine.
	shutdownErr := make(chan error, 1)
	go func() {
		shutdownErr <- c.HandleServerShutdown(nil)
	}()

	// Wait for HandleServerShutdown to return. With the bug, it returns
	// after the first handshake's error. With a fix, it returns after
	// all three handshakes complete.
	select {
	case <-shutdownErr:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleServerShutdown did not return")
	}

	// Every account must have been attempted, even though one handshake
	// failed. Otherwise that single failure silently took the rest of the
	// trader's accounts offline.
	attemptedMtx.Lock()
	got := len(attempted)
	attemptedMtx.Unlock()
	if got < len(keys) {
		t.Fatalf("expected re-subscribe to attempt all %d accounts, "+
			"only attempted %d — the loop bails on first error "+
			"and leaves later accounts silently un-subscribed",
			len(keys), got)
	}

	// Terminate the readIncomingStream goroutine that connectServerStream
	// spawned, so it doesn't leak past the test.
	stream.recv <- recvResult{err: io.EOF}
}
