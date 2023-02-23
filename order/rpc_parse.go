package order

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/tor"
)

// ParseOption defines a set of functional param options that can be used
// to modify our we parse orders based on some optional directives.
type ParseOption func(*parseOptions)

// parseOptions houses the set of functional options used to parse RPC orders.
type parseOptions struct {
	chanTypeSelector func() ChannelType
}

// ChanTypeSelector defines a function capable of selecting a channel type
// based on the version of an lnd node.
type ChanTypeSelector func() ChannelType

// WithDefaultChannelType allows a caller to select a default channel type
// based on the version of the lnd node attempting to create the order.
func WithDefaultChannelType(selector ChanTypeSelector) ParseOption {
	return func(o *parseOptions) {
		o.chanTypeSelector = selector
	}
}

// defaultParseOptions returns the set of default parse options.
func defaultParseOptions() *parseOptions {
	return &parseOptions{}
}

// ParseRPCOrder parses the incoming raw RPC order into the go native data
// types used in the order struct.
func ParseRPCOrder(version, leaseDuration uint32,
	details *poolrpc.Order, parseOpts ...ParseOption) (*Kit, error) {

	opts := defaultParseOptions()
	for _, newOpt := range parseOpts {
		newOpt(opts)
	}

	var nonce Nonce
	copy(nonce[:], details.OrderNonce)
	kit := NewKit(nonce)

	// If the user didn't provide a nonce, we generate one.
	if nonce == ZeroNonce {
		preimageBytes, err := randomPreimage()
		if err != nil {
			return nil, fmt.Errorf("cannot generate nonce: %v", err)
		}
		var preimage lntypes.Preimage
		copy(preimage[:], preimageBytes)
		kit = NewKitWithPreimage(preimage)
	}

	copy(kit.AcctKey[:], details.TraderKey)
	kit.AuctionType = AuctionType(details.AuctionType)
	kit.Version = Version(version)
	kit.FixedRate = details.RateFixed
	kit.Amt = btcutil.Amount(details.Amt)
	kit.MaxBatchFeeRate = chainfee.SatPerKWeight(
		details.MaxBatchFeeRateSatPerKw,
	)
	kit.Units = NewSupplyFromSats(kit.Amt)
	kit.UnitsUnfulfilled = kit.Units
	kit.LeaseDuration = leaseDuration

	switch {
	// If a min units match constraint was not provided, return an error.
	case details.MinUnitsMatch == 0:
		return nil, errors.New("min units match must be greater than 0")

	// The min units match must not exceed the total order units.
	case kit.AuctionType != BTCOutboundLiquidity &&
		details.MinUnitsMatch > uint32(kit.Units):

		return nil, errors.New("min units match must not exceed " +
			"total order units")
	}
	kit.MinUnitsMatch = SupplyUnit(details.MinUnitsMatch)

	switch details.ChannelType {
	// Default value, trader didn't specify a channel type.
	case auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_UNKNOWN:
		// If we have a chan type selector, we'll use that, otherwise
		// we'll just use peer dependent channels as default.
		if opts.chanTypeSelector != nil {
			kit.ChannelType = opts.chanTypeSelector()
		} else {
			kit.ChannelType = ChannelTypePeerDependent
		}

	case auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_PEER_DEPENDENT:
		kit.ChannelType = ChannelTypePeerDependent

	case auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_SCRIPT_ENFORCED:
		kit.ChannelType = ChannelTypeScriptEnforced

	default:
		return nil, fmt.Errorf("unhandled channel type %v",
			details.ChannelType)
	}

	if len(details.AllowedNodeIds) > 0 &&
		len(details.NotAllowedNodeIds) > 0 {

		return nil, errors.New("allowed and not allowed node ids set " +
			"at the same time")
	}

	allowedNodeIDs, err := UnmarshalNodeIDSlice(details.AllowedNodeIds)
	if err != nil {
		return nil, fmt.Errorf("invalid allowed_node_ids: %v", err)
	}
	kit.AllowedNodeIDs = allowedNodeIDs

	notAllowedNodeIDs, err := UnmarshalNodeIDSlice(
		details.NotAllowedNodeIds,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid not_allowed_node_ids: %v", err)
	}
	kit.NotAllowedNodeIDs = notAllowedNodeIDs

	kit.IsPublic = details.IsPublic

	return kit, nil
}

// parseNodeAddrs parses the set of address in strong format as returned over
// the RPC layer into a proper interface we can use.
func parseNodeAddrs(rpcAddrs []*auctioneerrpc.NodeAddress,
	orderIsAsk bool) ([]net.Addr, error) {

	if len(rpcAddrs) == 0 && orderIsAsk {
		return nil, fmt.Errorf("ask order doesn't include any node " +
			"addrs")
	}

	nodeAddrs := make([]net.Addr, 0, len(rpcAddrs))
	for _, rpcAddr := range rpcAddrs {
		var (
			addr net.Addr
			err  error
		)

		// Obtain the host to determine if this is a Tor address.
		host, _, err := net.SplitHostPort(rpcAddr.Addr)
		if err != nil {
			host = rpcAddr.Addr
		}

		switch {
		case tor.IsOnionHost(host):
			addr, err = parseOnionAddr(rpcAddr.Addr)

		default:
			addr, err = net.ResolveTCPAddr(
				rpcAddr.Network, rpcAddr.Addr,
			)
		}

		if err != nil {
			return nil, fmt.Errorf("unable to parse node "+
				"addr: %v", err)
		}

		nodeAddrs = append(nodeAddrs, addr)
	}

	return nodeAddrs, nil
}

// ParseRPCServerOrder parses the incoming raw RPC server order into the go
// native data types used in the order struct.
func ParseRPCServerOrder(version uint32, details *auctioneerrpc.ServerOrder,
	orderIsAsk bool, leaseDuration uint32) (*Kit, [33]byte, []net.Addr,
	[33]byte, error) {

	var (
		nonce       Nonce
		nodeKey     [33]byte
		multiSigKey [33]byte
	)

	copy(nonce[:], details.OrderNonce)
	kit := NewKit(nonce)
	kit.AuctionType = AuctionType(details.AuctionType)
	kit.Version = Version(version)
	kit.FixedRate = details.RateFixed
	kit.Amt = btcutil.Amount(details.Amt)
	kit.Units = NewSupplyFromSats(kit.Amt)
	kit.UnitsUnfulfilled = kit.Units
	kit.MaxBatchFeeRate = chainfee.SatPerKWeight(
		details.MaxBatchFeeRateSatPerKw,
	)
	kit.LeaseDuration = leaseDuration

	// If the user didn't provide a nonce, we generate one.
	if nonce == ZeroNonce {
		preimageBytes, err := randomPreimage()
		if err != nil {
			return nil, nodeKey, nil, multiSigKey,
				fmt.Errorf("cannot generate nonce: %v", err)
		}
		var preimage lntypes.Preimage
		copy(preimage[:], preimageBytes)
		kit = NewKitWithPreimage(preimage)
	}

	copy(kit.AcctKey[:], details.TraderKey)

	nodePubKey, err := btcec.ParsePubKey(details.NodePub)
	if err != nil {
		return nil, nodeKey, nil, multiSigKey,
			fmt.Errorf("unable to parse node pub key: %v",
				err)
	}
	copy(nodeKey[:], nodePubKey.SerializeCompressed())

	nodeAddrs, err := parseNodeAddrs(details.NodeAddr, orderIsAsk)
	if err != nil {
		return nil, nodeKey, nil, multiSigKey, err
	}

	multiSigPubkey, err := btcec.ParsePubKey(details.MultiSigKey)
	if err != nil {
		return nil, nodeKey, nodeAddrs, multiSigKey,
			fmt.Errorf("unable to parse multi sig pub key: %v", err)
	}
	copy(multiSigKey[:], multiSigPubkey.SerializeCompressed())

	switch details.ChannelType {
	// Default value, trader didn't specify a channel type.
	case auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_UNKNOWN:
		kit.ChannelType = ChannelTypePeerDependent

	case auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_PEER_DEPENDENT:
		kit.ChannelType = ChannelTypePeerDependent

	case auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_SCRIPT_ENFORCED:
		kit.ChannelType = ChannelTypeScriptEnforced

	default:
		return nil, nodeKey, nil, multiSigKey,
			fmt.Errorf("unhandled channel type %v", details.ChannelType)
	}

	return kit, nodeKey, nodeAddrs, multiSigKey, nil
}

// ParseRPCServerAsk parses the incoming raw RPC server ask into the go
// native data types used in the order struct.
func ParseRPCServerAsk(details *auctioneerrpc.ServerAsk) (*MatchedOrder, error) {
	var (
		o   = &MatchedOrder{}
		kit *Kit
		err error
	)
	kit, o.NodeKey, o.NodeAddrs, o.MultiSigKey, err = ParseRPCServerOrder(
		details.Version, details.Details, true,
		details.LeaseDurationBlocks,
	)
	if err != nil {
		return nil, err
	}

	kit.LeaseDuration = details.LeaseDurationBlocks

	announcement := ChannelAnnouncementConstraints(
		details.AnnouncementConstraints,
	)
	confirmations := ChannelConfirmationConstraints(
		details.ConfirmationConstraints,
	)
	o.Order = &Ask{
		Kit:                     *kit,
		AnnouncementConstraints: announcement,
		ConfirmationConstraints: confirmations,
	}

	return o, nil
}

// ParseRPCServerBid parses the incoming raw RPC server bid into the go
// native data types used in the order struct.
func ParseRPCServerBid(details *auctioneerrpc.ServerBid) (*MatchedOrder, error) {
	var (
		o   = &MatchedOrder{}
		kit *Kit
		err error
	)
	kit, o.NodeKey, o.NodeAddrs, o.MultiSigKey, err = ParseRPCServerOrder(
		details.Version, details.Details, false,
		details.LeaseDurationBlocks,
	)
	if err != nil {
		return nil, err
	}

	o.Order = &Bid{
		Kit:                *kit,
		SelfChanBalance:    btcutil.Amount(details.SelfChanBalance),
		UnannouncedChannel: details.UnannouncedChannel,
		ZeroConfChannel:    details.ZeroConfChannel,
	}

	return o, nil
}

// ParseRPCBatch parses the incoming raw RPC batch into the go native data types
// used by the order manager.
func ParseRPCBatch(prepareMsg *auctioneerrpc.OrderMatchPrepare) (*Batch,
	error) {

	b := &Batch{
		Version:        BatchVersion(prepareMsg.BatchVersion),
		MatchedOrders:  make(map[Nonce][]*MatchedOrder),
		BatchTX:        &wire.MsgTx{},
		ClearingPrices: make(map[uint32]FixedRatePremium),
		HeightHint:     prepareMsg.BatchHeightHint,
	}

	// Parse matched orders market by market.
	for leaseDuration, matchedMarket := range prepareMsg.MatchedMarkets {
		orders := matchedMarket.MatchedOrders
		for ourOrderHex, rpcMatchedOrders := range orders {
			var ourOrder Nonce
			ourOrderBytes, err := hex.DecodeString(ourOrderHex)
			if err != nil {
				return nil, fmt.Errorf("error parsing nonce: "+
					"%v", err)
			}
			copy(ourOrder[:], ourOrderBytes)
			matchedOrders, err := ParseRPCMatchedOrders(
				rpcMatchedOrders,
			)
			if err != nil {
				return nil, fmt.Errorf("error parsing matched "+
					"order: %v", err)
			}

			// It is imperative that all matched orders have the
			// correct lease duration. Otherwise assumptions in the
			// batch verifier won't be correct. That's why we
			// validate this here already.
			for _, matchedOrder := range matchedOrders {
				details := matchedOrder.Order.Details()
				if details.LeaseDuration != leaseDuration {
					return nil, fmt.Errorf("matched order "+
						"%s has incorrect lease "+
						"duration %d for bucket %d",
						details.Nonce(),
						details.LeaseDuration,
						leaseDuration)
				}
			}

			b.MatchedOrders[ourOrder] = matchedOrders
		}

		b.ClearingPrices[leaseDuration] = FixedRatePremium(
			matchedMarket.ClearingPriceRate,
		)
	}

	// Parse account diff.
	for _, diff := range prepareMsg.ChargedAccounts {
		var acctKeyRaw [33]byte
		acctKey, err := btcec.ParsePubKey(diff.TraderKey)
		if err != nil {
			return nil, fmt.Errorf("error parsing account key: %v",
				err)
		}
		copy(acctKeyRaw[:], acctKey.SerializeCompressed())
		b.AccountDiffs = append(
			b.AccountDiffs, &AccountDiff{
				AccountKeyRaw: acctKeyRaw,
				AccountKey:    acctKey,
				EndingState:   diff.EndingState,
				EndingBalance: btcutil.Amount(diff.EndingBalance),
				OutpointIndex: diff.OutpointIndex,
				NewExpiry:     diff.NewExpiry,
				NewVersion:    account.Version(diff.NewVersion),
			},
		)
	}

	// Parse batch transaction.
	err := b.BatchTX.Deserialize(bytes.NewReader(
		prepareMsg.BatchTransaction,
	))
	if err != nil {
		return nil, fmt.Errorf("error parsing batch TX: %v", err)
	}

	// Convert clearing price, fee rate and rebate.
	b.BatchTxFeeRate = chainfee.SatPerKWeight(prepareMsg.FeeRateSatPerKw)
	b.FeeRebate = btcutil.Amount(prepareMsg.FeeRebateSat)

	// Parse the execution fee.
	if prepareMsg.ExecutionFee == nil {
		return nil, fmt.Errorf("execution fee missing")
	}
	b.ExecutionFee = terms.NewLinearFeeSchedule(
		btcutil.Amount(prepareMsg.ExecutionFee.BaseFee),
		btcutil.Amount(prepareMsg.ExecutionFee.FeeRate),
	)

	// Parse the batch ID as public key just to make sure it's valid.
	_, err = btcec.ParsePubKey(prepareMsg.BatchId)
	if err != nil {
		return nil, fmt.Errorf("error parsing batch ID: %v", err)
	}
	copy(b.ID[:], prepareMsg.BatchId)

	return b, nil
}

// ParseRPCMatchedOrders parses the incoming raw RPC matched orders into the go
// native structs used by the order manager.
func ParseRPCMatchedOrders(orders *auctioneerrpc.MatchedOrder) ([]*MatchedOrder,
	error) {

	var result []*MatchedOrder
	// The only thing we can check in this step is that not both matched
	// bids and matched asks are set at the same time as that wouldn't make
	// sense. Everything else is checked at a later stage when we know more
	// about our order that was matched against.
	switch {
	case len(orders.MatchedAsks) > 0 && len(orders.MatchedBids) > 0:
		return nil, fmt.Errorf("order cannot match both asks and bids")

	case len(orders.MatchedAsks) > 0:
		for _, ask := range orders.MatchedAsks {
			matchedAsk, err := ParseRPCServerAsk(ask.Ask)
			if err != nil {
				return nil, fmt.Errorf("error parsing server "+
					"ask: %v", err)
			}
			matchedAsk.UnitsFilled = SupplyUnit(ask.UnitsFilled)

			result = append(result, matchedAsk)
		}

	case len(orders.MatchedBids) > 0:
		for _, bid := range orders.MatchedBids {
			matchedBid, err := ParseRPCServerBid(bid.Bid)
			if err != nil {
				return nil, fmt.Errorf("error parsing server "+
					"bid: %v", err)
			}
			matchedBid.UnitsFilled = SupplyUnit(bid.UnitsFilled)

			result = append(result, matchedBid)
		}
	}

	return result, nil
}

// ParseRPCSign parses the incoming raw OrderMatchSignBegin into the go native
// structs used by the order manager.
func ParseRPCSign(signMsg *auctioneerrpc.OrderMatchSignBegin) (AccountNonces,
	[]*wire.TxOut, error) {

	nonces := make(AccountNonces, len(signMsg.ServerNonces))
	for acctKeyHex, nonceBytes := range signMsg.ServerNonces {
		var acctKey [btcec.PubKeyBytesLenCompressed]byte

		if len(acctKeyHex) != hex.EncodedLen(len(acctKey)) {
			return nil, nil, fmt.Errorf("invalid account key " +
				"length in server nonces")
		}
		if len(nonceBytes) != musig2.PubNonceSize {
			return nil, nil, fmt.Errorf("invalid pub nonce " +
				"length in server nonces")
		}

		acctKeyBytes, err := hex.DecodeString(acctKeyHex)
		if err != nil {
			return nil, nil, fmt.Errorf("error hex decoding "+
				"account key: %v", err)
		}
		copy(acctKey[:], acctKeyBytes)

		var pubNonces [musig2.PubNonceSize]byte
		copy(pubNonces[:], nonceBytes)

		nonces[acctKey] = pubNonces
	}

	prevOutputs := make([]*wire.TxOut, len(signMsg.PrevOutputs))
	for idx, rpcPrevOut := range signMsg.PrevOutputs {
		prevOutputs[idx] = &wire.TxOut{
			Value:    int64(rpcPrevOut.Value),
			PkScript: rpcPrevOut.PkScript,
		}
	}

	return nonces, prevOutputs, nil
}

// MarshalNodeIDSlice returns a flattened version of an slice of node ids to be
// used in rpc serialization.
func MarshalNodeIDSlice(nodeIDs [][33]byte) [][]byte {
	res := make([][]byte, 0, len(nodeIDs))

	for i := range nodeIDs {
		nodeID := make([]byte, 33)
		copy(nodeID, nodeIDs[i][:])

		res = append(res, nodeID)
	}

	return res
}

// UnmarshalNodeIDSlice returns a slice of node ids from a flatten version.
func UnmarshalNodeIDSlice(slice [][]byte) ([][33]byte, error) {
	nodeIDs := make([][33]byte, len(slice))
	for idx := range slice {
		// Check that the node id pub key is in the correct format.
		if len(slice[idx]) != 33 {
			return nil, fmt.Errorf("invalid node_id length: %x",
				slice[idx])
		}

		// Check that the node id pub key is a valid key.
		if _, err := btcec.ParsePubKey(slice[idx]); err != nil {
			return nil, fmt.Errorf("invalid node_id: %x",
				slice[idx])
		}

		copy(nodeIDs[idx][:], slice[idx])
	}

	return nodeIDs, nil
}

// randomPreimage creates a new preimage from a random number generator.
func randomPreimage() ([]byte, error) {
	var nonce Nonce
	_, err := rand.Read(nonce[:])
	if err != nil {
		return nil, err
	}
	return nonce[:], nil
}
