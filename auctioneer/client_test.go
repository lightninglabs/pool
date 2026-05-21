package auctioneer

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/order"
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

// TestConnectAndAuthenticateCleansUpOnAccountDoesNotExist drives a full
// connectAndAuthenticate call in recovery mode against a scripted fake stream
// that responds with ACCOUNT_DOES_NOT_EXIST after the client's Subscribe. It
// asserts the subscription entry is removed from c.subscribedAccts on return.
//
// Regression: previously the entry was added to the map before authenticate
// ran (so readIncomingStream could route the server's Challenge/Error back
// to it) and was never removed on error paths. A later StartAccountSubscription
// for the same account — typically when handleStateOpen runs after on-chain
// confirmation — would hit the "already subscribed" early-return guard at the
// top of connectAndAuthenticate and silently no-op without sending a fresh
// Commit. The per-account 3-way handshake never ran and the trader stayed
// filtered as offline at matching time until the process restarted.
func TestConnectAndAuthenticateCleansUpOnAccountDoesNotExist(t *testing.T) {
	t.Parallel()

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

	// Step 4: server responds with ACCOUNT_DOES_NOT_EXIST (the realistic
	// case where RecoverAccounts probes a key the auctioneer hasn't yet
	// seen on chain).
	var pubKey [33]byte
	copy(pubKey[:], testAccountDesc.PubKey.SerializeCompressed())
	stream.recv <- recvResult{
		msg: &auctioneerrpc.ServerAuctionMessage{
			Msg: &auctioneerrpc.ServerAuctionMessage_Error{
				Error: &auctioneerrpc.SubscribeError{
					ErrorCode: auctioneerrpc.SubscribeError_ACCOUNT_DOES_NOT_EXIST,
					TraderKey: pubKey[:],
				},
			},
		},
	}

	// Step 5: connectAndAuthenticate should return (sub, false, nil) ...
	var res result
	select {
	case res = <-resCh:
	case <-time.After(defaultTimeout):
		t.Fatal("connectAndAuthenticate did not return")
	}
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if res.canRecover {
		t.Fatal("expected canRecover=false on ACCOUNT_DOES_NOT_EXIST")
	}
	if res.sub == nil {
		t.Fatal("expected non-nil subscription")
	}

	// ... and the subscription must NOT be left in the map. A later
	// StartAccountSubscription for this account would otherwise hit the
	// "already subscribed" guard and silently no-op without ever sending
	// a fresh Commit.
	c.subscribedAcctsMtx.Lock()
	_, present := c.subscribedAccts[pubKey]
	c.subscribedAcctsMtx.Unlock()
	if present {
		t.Fatal("subscribedAccts entry was not cleaned up after " +
			"ACCOUNT_DOES_NOT_EXIST; later subscribes for the " +
			"same account would silently no-op")
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
