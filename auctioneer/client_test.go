package auctioneer

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/lightninglabs/pool/auctioneerrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeServerStream is a minimal implementation of
// ChannelAuctioneer_SubscribeBatchAuctionClient that returns predetermined
// results from Recv. It is only sufficient for driving the client's read loop.
type fakeServerStream struct {
	grpc.ClientStream

	recv chan recvResult
}

type recvResult struct {
	msg *auctioneerrpc.ServerAuctionMessage
	err error
}

func (s *fakeServerStream) Send(*auctioneerrpc.ClientAuctionMessage) error {
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
