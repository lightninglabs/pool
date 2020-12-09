package funding

import (
	"context"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/order"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// CancelPendingFundingShims cancels all funding shims we registered when
// preparing for the orders in a batch. This should be called if for any reason
// we need to reject the batch, so we're able to process any subsequent modified
// batches.
func CancelPendingFundingShims(matchedOrders map[order.Nonce][]*order.MatchedOrder,
	baseClient BaseClient, fetchOrder order.Fetcher) error {

	// Since we support partial matches, a given bid of ours could've been
	// matched with multiple asks, so we'll iterate through all those to
	// ensure we unregister all the created shims.
	ctxb := context.Background()
	for ourOrderNonce, matchedOrders := range matchedOrders {
		ourOrder, err := fetchOrder(ourOrderNonce)
		if err != nil {
			return err
		}

		orderIsAsk := ourOrder.Type() == order.TypeAsk

		// If the order as an ask, then we don't need to do anything,
		// as we only register funding shims for incoming channels (so
		// buys).
		if orderIsAsk {
			continue
		}

		// For each ask order that was matched with this bid, we'll
		// re-derive the pending chan ID key used, then attempt to
		// unregister it.
		for _, matchedOrder := range matchedOrders {
			bidNonce := ourOrder.Nonce()
			askNonce := matchedOrder.Order.Nonce()
			pendingChanID := order.PendingChanKey(
				askNonce, bidNonce,
			)

			cancelShimMsg := &lnrpc.FundingTransitionMsg_ShimCancel{
				ShimCancel: &lnrpc.FundingShimCancel{
					PendingChanId: pendingChanID[:],
				},
			}

			_, err = baseClient.FundingStateStep(
				ctxb, &lnrpc.FundingTransitionMsg{
					Trigger: cancelShimMsg,
				},
			)
			if err != nil {
				log.Warnf("Unable to unregister funding shim "+
					"(pendingChanID=%x) for order=%v",
					pendingChanID[:], bidNonce)
			}
		}
	}

	return nil
}

// AbandonCanceledChannels removes all channels from lnd's channel database that
// were created for an iteration of a batch that never made it to chain in the
// provided configuration. This should be called whenever a batch is replaced
// with an updated version because some traders were offline or rejected the
// batch. If a non-nil error is returned, something with reading the local order
// or extracting the channel outpoint went wrong and we should fail hard. If the
// channel cannot be abandoned for some reason, the error is just logged but not
// returned.
func AbandonCanceledChannels(matchedOrders map[order.Nonce][]*order.MatchedOrder,
	batchTx *wire.MsgTx, wallet lndclient.WalletKitClient,
	baseClient BaseClient, fetchOrder order.Fetcher) error {

	// Since we support partial matches, a given bid of ours could've been
	// matched with multiple asks, so we'll iterate through all those to
	// ensure we remove all channels that never made it to chain.
	ctxb := context.Background()
	txHash := batchTx.TxHash()
	for ourOrderNonce, matchedOrders := range matchedOrders {
		ourOrder, err := fetchOrder(ourOrderNonce)
		if err != nil {
			return err
		}

		// For each ask order that was matched with this bid, we'll
		// locate the channel outpoint then abandon it from lnd's
		// channel database.
		for _, matchedOrder := range matchedOrders {
			_, idx, err := order.ChannelOutput(
				batchTx, wallet, ourOrder, matchedOrder,
			)
			if err != nil {
				return fmt.Errorf("error locating channel "+
					"outpoint: %v", err)
			}

			channelPoint := &lnrpc.ChannelPoint{
				OutputIndex: idx,
				FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
					FundingTxidBytes: txHash[:],
				},
			}
			_, err = baseClient.AbandonChannel(
				ctxb, &lnrpc.AbandonChannelRequest{
					ChannelPoint:           channelPoint,
					PendingFundingShimOnly: true,
				},
			)
			const notFoundErr = "unable to find closed channel"
			if err != nil {
				// If the channel was never created in the first
				// place, it might just not exist. Therefore we
				// ignore the "not found" error but fail on any
				// other error.
				if !strings.Contains(err.Error(), notFoundErr) {
					log.Errorf("Unexpected error when "+
						"trying to clean up pending "+
						"channels: %v", err)
					return err
				}

				log.Debugf("Cleaning up incomplete/replaced "+
					"pending channel in lnd was "+
					"unsuccessful for order=%v "+
					"(channel_point=%v:%d), assuming "+
					"timeout when funding: %v", txHash, idx,
					ourOrderNonce, err)
			}
		}
	}

	return nil
}
