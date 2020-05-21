package order

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ParseRPCOrder parses the incoming raw RPC order into the go native data
// types used in the order struct.
func ParseRPCOrder(version uint32, details *clmrpc.Order) (*Kit, error) {
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

	copy(kit.AcctKey[:], details.UserSubKey)
	kit.Version = Version(version)
	kit.FixedRate = uint32(details.RateFixed)
	kit.Amt = btcutil.Amount(details.Amt)
	kit.FundingFeeRate = chainfee.SatPerKWeight(details.FundingFeeRate)
	kit.Units = NewSupplyFromSats(kit.Amt)
	kit.UnitsUnfulfilled = kit.Units
	return kit, nil
}

// ParseRPCServerOrder parses the incoming raw RPC server order into the go
// native data types used in the order struct.
func ParseRPCServerOrder(version uint32, details *clmrpc.ServerOrder) (*Kit,
	[33]byte, []net.Addr, [33]byte, error) {

	var (
		nonce       Nonce
		nodeKey     [33]byte
		nodeAddrs   = make([]net.Addr, 0, len(details.NodeAddr))
		multiSigKey [33]byte
	)

	copy(nonce[:], details.OrderNonce)
	kit := NewKit(nonce)
	kit.Version = Version(version)
	kit.FixedRate = uint32(details.RateFixed)
	kit.Amt = btcutil.Amount(details.Amt)
	kit.Units = NewSupplyFromSats(kit.Amt)
	kit.UnitsUnfulfilled = kit.Units
	kit.FundingFeeRate = chainfee.SatPerKWeight(
		details.FundingFeeRateSatPerKw,
	)

	// If the user didn't provide a nonce, we generate one.
	if nonce == ZeroNonce {
		preimageBytes, err := randomPreimage()
		if err != nil {
			return nil, nodeKey, nodeAddrs, multiSigKey,
				fmt.Errorf("cannot generate nonce: %v", err)
		}
		var preimage lntypes.Preimage
		copy(preimage[:], preimageBytes)
		kit = NewKitWithPreimage(preimage)
	}

	copy(kit.AcctKey[:], details.UserSubKey)

	nodePubKey, err := btcec.ParsePubKey(details.NodePub, btcec.S256())
	if err != nil {
		return nil, nodeKey, nodeAddrs, multiSigKey,
			fmt.Errorf("unable to parse node pub key: %v",
				err)
	}
	copy(nodeKey[:], nodePubKey.SerializeCompressed())
	if len(details.NodeAddr) == 0 {
		return nil, nodeKey, nodeAddrs, multiSigKey,
			fmt.Errorf("invalid node addresses")
	}
	for _, rpcAddr := range details.NodeAddr {
		addr, err := net.ResolveTCPAddr(rpcAddr.Network, rpcAddr.Addr)
		if err != nil {
			return nil, nodeKey, nodeAddrs, multiSigKey,
				fmt.Errorf("unable to parse node ddr: %v", err)
		}
		nodeAddrs = append(nodeAddrs, addr)
	}
	multiSigPubkey, err := btcec.ParsePubKey(
		details.MultiSigKey, btcec.S256(),
	)
	if err != nil {
		return nil, nodeKey, nodeAddrs, multiSigKey,
			fmt.Errorf("unable to parse multi sig pub key: %v", err)
	}
	copy(multiSigKey[:], multiSigPubkey.SerializeCompressed())

	return kit, nodeKey, nodeAddrs, multiSigKey, nil
}

// ParseRPCServerAsk parses the incoming raw RPC server ask into the go
// native data types used in the order struct.
func ParseRPCServerAsk(details *clmrpc.ServerAsk) (*MatchedOrder, error) {
	var (
		o   = &MatchedOrder{}
		kit *Kit
		err error
	)
	kit, o.NodeKey, o.NodeAddrs, o.MultiSigKey, err =
		ParseRPCServerOrder(details.Version, details.Details)
	if err != nil {
		return nil, err
	}
	o.Order = &Ask{
		Kit:         *kit,
		MaxDuration: uint32(details.MaxDurationBlocks),
	}
	return o, nil
}

// ParseRPCServerBid parses the incoming raw RPC server bid into the go
// native data types used in the order struct.
func ParseRPCServerBid(details *clmrpc.ServerBid) (*MatchedOrder, error) {
	var (
		o   = &MatchedOrder{}
		kit *Kit
		err error
	)
	kit, o.NodeKey, o.NodeAddrs, o.MultiSigKey, err =
		ParseRPCServerOrder(details.Version, details.Details)
	if err != nil {
		return nil, err
	}
	o.Order = &Bid{
		Kit:         *kit,
		MinDuration: uint32(details.MinDurationBlocks),
	}
	return o, nil
}

// ParseRPCBatch parses the incoming raw RPC batch into the go native data types
// used by the order manager.
func ParseRPCBatch(prepareMsg *clmrpc.OrderMatchPrepare) (*Batch,
	error) {

	b := &Batch{
		Version:       BatchVersion(prepareMsg.BatchVersion),
		MatchedOrders: make(map[Nonce][]*MatchedOrder),
		BatchTX:       &wire.MsgTx{},
	}

	// Parse matched orders.
	for ourOrderHex, rpcMatchedOrders := range prepareMsg.MatchedOrders {
		var ourOrder Nonce
		ourOrderBytes, err := hex.DecodeString(ourOrderHex)
		if err != nil {
			return nil, fmt.Errorf("error parsing nonce: %v", err)
		}
		copy(ourOrder[:], ourOrderBytes)
		b.MatchedOrders[ourOrder], err = ParseRPCMatchedOrders(
			rpcMatchedOrders,
		)
		if err != nil {
			return nil, fmt.Errorf("error parsing matched order: "+
				"%v", err)
		}
	}

	// Parse account diff.
	for _, diff := range prepareMsg.ChargedAccounts {
		var acctKeyRaw [33]byte
		acctKey, err := btcec.ParsePubKey(diff.UserSubKey, btcec.S256())
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
	b.ClearingPrice = FixedRatePremium(prepareMsg.ClearingPriceRate)
	b.BatchTxFeeRate = chainfee.SatPerKWeight(prepareMsg.FeeRateSatPerKw)
	b.FeeRebate = btcutil.Amount(prepareMsg.FeeRebateSat)

	// Parse the execution fee.
	if prepareMsg.ExecutionFee == nil {
		return nil, fmt.Errorf("execution fee missing")
	}
	b.ExecutionFee = NewLinearFeeSchedule(
		btcutil.Amount(prepareMsg.ExecutionFee.BaseFee),
		btcutil.Amount(prepareMsg.ExecutionFee.FeeRate),
	)

	// Parse the batch ID as public key just to make sure it's valid.
	_, err = btcec.ParsePubKey(prepareMsg.BatchId, btcec.S256())
	if err != nil {
		return nil, fmt.Errorf("error parsing batch ID: %v", err)
	}
	copy(b.ID[:], prepareMsg.BatchId)

	return b, nil
}

// ParseRPCMatchedOrders parses the incoming raw RPC matched orders into the go
// native structs used by the order manager.
func ParseRPCMatchedOrders(orders *clmrpc.MatchedOrder) ([]*MatchedOrder,
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

// randomPreimage creates a new preimage from a random number generator.
func randomPreimage() ([]byte, error) {
	var nonce Nonce
	_, err := rand.Read(nonce[:])
	if err != nil {
		return nil, err
	}
	return nonce[:], nil
}
