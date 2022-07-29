package pool

import (
	"context"
	"fmt"
	"sync"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/order"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ChannelAcceptor is a type that adds an RPC level interceptor for accepting
// channels in lnd. Its main task is to validate the self channel balance (or
// as it's known in the LN lingo: push amount) of incoming channels against the
// expected (and paid for!) amount in the order.
type ChannelAcceptor struct {
	lightning lndclient.LightningClient

	expectedChans    map[[32]byte]*order.Bid
	expectedChansMtx sync.Mutex

	acceptorCancel func()
	errChan        chan error
	quit           chan struct{}
	wg             sync.WaitGroup
}

// NewChannelAcceptor creates a new channel acceptor with the given lnd client.
func NewChannelAcceptor(lightning lndclient.LightningClient) *ChannelAcceptor {
	return &ChannelAcceptor{
		lightning:     lightning,
		expectedChans: make(map[[32]byte]*order.Bid),
		quit:          make(chan struct{}),
	}
}

// Start starts the channel acceptor and subscribes to receive all incoming
// channel events of lnd.
func (s *ChannelAcceptor) Start(errChan chan error) error {
	s.errChan = errChan

	ctxc := context.Background()
	ctxc, s.acceptorCancel = context.WithCancel(ctxc)

	rpcErrChan, err := s.lightning.ChannelAcceptor(
		ctxc, order.DefaultBatchStepTimeout, s.acceptChannel,
	)
	if err != nil {
		return err
	}

	s.wg.Add(1)
	go s.subscribe(rpcErrChan)

	return nil
}

// subscribe subscribes to errors coming from the RPC error channel and forwards
// them to our main error channel.
func (s *ChannelAcceptor) subscribe(rpcErrChan chan error) {
	defer s.wg.Done()

	for {
		select {
		case err := <-rpcErrChan:
			select {
			case s.errChan <- err:
			case <-s.quit:
			}

		case <-s.quit:
			s.acceptorCancel()

			return
		}
	}
}

// Stop shuts down the channel acceptor.
func (s *ChannelAcceptor) Stop() {
	s.acceptorCancel()
	close(s.quit)

	s.wg.Wait()
}

// ShimRegistered is a function that should be called whenever a funding shim
// is created for a bid order where we expect an incoming channel at any moment.
func (s *ChannelAcceptor) ShimRegistered(bid *order.Bid, pid [32]byte) {
	s.expectedChansMtx.Lock()
	defer s.expectedChansMtx.Unlock()

	s.expectedChans[pid] = bid
}

// ShimRemoved is a function that should be called whenever a funding shim is
// cleaned up and we no longer expect an incoming channel for it.
func (s *ChannelAcceptor) ShimRemoved(bid *order.Bid) {
	s.expectedChansMtx.Lock()
	defer s.expectedChansMtx.Unlock()

	for pid, expectedBid := range s.expectedChans {
		if expectedBid.Nonce() == bid.Nonce() {
			delete(s.expectedChans, pid)
		}
	}
}

// acceptChannel is the callback that is invoked each time a new incoming
// channel message is received in lnd. We inspect it here and if it corresponds
// to a pending channel ID that we have an expectation for, we check whether the
// self chan balance (=push amount) is correct.
func (s *ChannelAcceptor) acceptChannel(_ context.Context,
	req *lndclient.AcceptorRequest) (*lndclient.AcceptorResponse, error) {

	s.expectedChansMtx.Lock()
	defer s.expectedChansMtx.Unlock()

	expectedChanBid, ok := s.expectedChans[req.PendingChanID]

	// It's not a channel we've registered within the funding manager so we
	// just accept it to not interfere with the normal node operation.
	if !ok {
		return &lndclient.AcceptorResponse{Accept: true}, nil
	}

	// The push amount in the acceptor request is in milli sats, we need to
	// convert it first.
	pushAmtSat := lnwire.MilliSatoshi(req.PushAmt).ToSatoshis()

	// Push amount must be exactly what we expect. Otherwise the asker could
	// be trying to cheat.
	if expectedChanBid.SelfChanBalance != pushAmtSat {
		return &lndclient.AcceptorResponse{
			Accept: false,
			Error: fmt.Sprintf("invalid push amount %v",
				req.PushAmt),
		}, nil
	}

	switch expectedChanBid.ChannelType {
	// The bid doesn't have specific requirements for the channel type.
	case order.ChannelTypePeerDependent:
		break

	// The bid expects a channel type that enforces the channel lease
	// maturity in its output scripts.
	case order.ChannelTypeScriptEnforced:
		if req.CommitmentType == nil {
			return &lndclient.AcceptorResponse{
				Accept: false,
				Error:  "expected explicit channel negotiation",
			}, nil
		}

		switch *req.CommitmentType {
		case lnwallet.CommitmentTypeScriptEnforcedLease:
		default:
			return &lndclient.AcceptorResponse{
				Accept: false,
				Error: "expected script enforced channel " +
					"lease commitment type",
			}, nil
		}

	default:
		log.Warnf("Unhandled channel type %v for bid %v",
			expectedChanBid.ChannelType, expectedChanBid.Nonce())
		return &lndclient.AcceptorResponse{
			Accept: false,
			Error:  "internal error",
		}, nil
	}

	fundingFlags := lnwire.FundingFlag(req.ChannelFlags)
	isPrivateChan := fundingFlags&lnwire.FFAnnounceChannel == 0

	// Check that the new channel is announced/unannounced as expected.
	if isPrivateChan != expectedChanBid.UnannouncedChannel {
		var errMsg string
		errTemplate := "expected an %s channel but received an %s one"

		if expectedChanBid.UnannouncedChannel {
			errMsg = fmt.Sprintf(errTemplate, "unannounced",
				"announced")
		} else {
			errMsg = fmt.Sprintf(errTemplate, "announced",
				"unannounced")
		}

		return &lndclient.AcceptorResponse{
			Accept: false,
			Error:  errMsg,
		}, nil
	}

	// Check that the channel is a zero conf channel if we were expecting
	// one.
	if expectedChanBid.ZeroConfChannel {
		if !req.WantsZeroConf {
			return &lndclient.AcceptorResponse{
				Accept: false,
				Error:  "expected zero conf channel",
			}, nil
		}
		return &lndclient.AcceptorResponse{
			Accept:         true,
			MinAcceptDepth: 0,
			ZeroConf:       true,
		}, nil
	}

	return &lndclient.AcceptorResponse{
		Accept: true,
	}, nil
}
