package order

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/internal/test"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

var (
	_, startBatchKey = btcec.PrivKeyFromBytes([]byte{0x01})

	_, acctKeyBig = btcec.PrivKeyFromBytes([]byte{0x02})

	_, acctKeySmall = btcec.PrivKeyFromBytes([]byte{0x03})

	nodePubkey            = [33]byte{03, 77, 44, 55}
	execFeeBase           = btcutil.Amount(1_100)
	execFeeRate           = btcutil.Amount(50)
	clearingPrice         = FixedRatePremium(5000)
	leaseDuration         = uint32(1000)
	stateRecreated        = auctioneerrpc.AccountDiff_OUTPUT_RECREATED
	stateExtendedOffchain = auctioneerrpc.AccountDiff_OUTPUT_DUST_EXTENDED_OFFCHAIN
	extendAccountExpiry   = 1000
)

func TestBatchVerifier(t *testing.T) {
	t.Parallel()

	const bestHeight = 1337
	var (
		walletKit   = test.NewMockWalletKit()
		batchID     BatchID
		acctIDBig   [33]byte
		acctIDSmall [33]byte
	)
	copy(batchID[:], startBatchKey.SerializeCompressed())
	copy(acctIDBig[:], acctKeyBig.SerializeCompressed())
	copy(acctIDSmall[:], acctKeySmall.SerializeCompressed())

	// All the test cases we want to run. The doVerify function gets passed
	// in a batch that does pass validation and represents the happy path.
	// The test cases can manipulate the "good" batch specifically to
	// trigger validation edge cases.
	testCases := []struct {
		name         string
		batchVersion BatchVersion
		expectedErr  string
		doVerify     func(BatchVerifier, *Ask, *Bid, *Bid, *Batch) error
	}{
		{
			name:         "version mismatch",
			batchVersion: DefaultBatchVersion,
			expectedErr: NewErrVersionMismatch(
				DefaultBatchVersion, 999,
			).Error(),
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				return v.Verify(&Batch{Version: 999}, bestHeight)
			},
		},
		{
			name:         "invalid order",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "not found",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				arr := make([]*MatchedOrder, 0)
				b.MatchedOrders[Nonce{99, 99}] = arr
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "invalid order type",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "matched same type orders",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.MatchedOrders[a.nonce] = append(
					b.MatchedOrders[a.nonce],
					&MatchedOrder{
						Order: a,
					},
				)
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "invalid node pubkey",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "other order is an order from our node",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.MatchedOrders[a.nonce][0].NodeKey = nodePubkey
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "ask max duration larger than bid",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "duration not overlapping",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				a.LeaseDuration = 100
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "ask fixed rate larger than bid",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "ask price greater than bid price",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				a.FixedRate = 20000
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "bid min duration larger than ask",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "duration not overlapping",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				delete(b.MatchedOrders, a.nonce)
				b2.LeaseDuration = 5000
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "bid fixed rate smaller than ask",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "ask price greater than bid price",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				delete(b.MatchedOrders, a.nonce)
				a.FixedRate = b1.FixedRate + 1
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "channel output not found, wrong value",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "no channel output found in batch tx for",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.BatchTX.TxOut[0].Value = 123
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "channel output not found, wrong script",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "no channel output found in batch tx for",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.BatchTX.TxOut[0].PkScript = []byte{99, 88}
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "invalid units filled",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "invalid units to be filled for order",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.BatchTX.TxOut[0].Value = 900_000
				b.MatchedOrders[a.nonce][0].UnitsFilled = 9
				b.MatchedOrders[b1.nonce][0].UnitsFilled = 9
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "invalid min units match",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "units, but minimum is",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				a.MinUnitsMatch = b1.MinUnitsMatch * 100
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "invalid funding TX fee rate",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "server sent unexpected ending balance",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.BatchTxFeeRate *= 2
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "invalid clearing price bid",
			batchVersion: DefaultBatchVersion,
			expectedErr:  "below clearing price",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				delete(b.MatchedOrders, a.nonce)
				b1.FixedRate = uint32(clearingPrice) - 1
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:        "invalid clearing price ask",
			expectedErr: "above clearing price",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				// To correctly test this case, we'll need to
				// create a custom match. Create an ask with a
				// matching price as the highest bid. These two
				// will be matched.
				b.MatchedOrders = make(map[Nonce][]*MatchedOrder)
				ask := &Ask{
					Kit: newKitFromTemplate(Nonce{0x04}, &Kit{
						MultiSigKeyLocator: keychain.KeyLocator{
							Index: 0,
						},
						Units:            4,
						UnitsUnfulfilled: 4,
						AcctKey:          acctIDSmall,
						FixedRate:        b1.FixedRate,
						LeaseDuration:    leaseDuration,
					}),
				}
				v.(*batchVerifier).orderStore.(*mockStore).orders[ask.nonce] = ask
				b.MatchedOrders[ask.nonce] = []*MatchedOrder{
					{
						Order:       b1,
						UnitsFilled: 2,
						MultiSigKey: deriveRawKey(
							t, walletKit,
							b1.MultiSigKeyLocator,
						),
					},
				}

				// Match the next highest bid with the remaining
				// ask.
				a.FixedRate = b2.FixedRate
				b.MatchedOrders[a.nonce] = []*MatchedOrder{
					{
						Order:       b2,
						UnitsFilled: 2,
						MultiSigKey: deriveRawKey(
							t, walletKit,
							b2.MultiSigKeyLocator,
						),
					},
				}

				// Verification should fail as the first match
				// has an ask with a price greater than the
				// clearing price.
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:        "invalid execution fee rate",
			expectedErr: "server sent unexpected ending balance",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.ExecutionFee = terms.NewLinearFeeSchedule(1, 1)
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:        "invalid ending state",
			expectedErr: "diff is incorrect: unexpected state",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.ExecutionFee = terms.NewLinearFeeSchedule(0, 0)
				b.BatchTX.TxOut[2].Value += 2220
				b.AccountDiffs[0].EndingBalance += 2220
				b.AccountDiffs[1].EndingBalance += 2220
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:        "invalid ending output",
			expectedErr: "diff is incorrect: outpoint index",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.ExecutionFee = terms.NewLinearFeeSchedule(0, 0)
				b.BatchTX.TxOut[2].Value += 2220
				b.AccountDiffs[0].EndingBalance += 2220
				b.AccountDiffs[1].EndingBalance += 2220
				b.AccountDiffs[1].EndingState = stateRecreated
				return v.Verify(b, bestHeight)
			},
		},
		{
			name: "self chan balance not added to funding " +
				"output",
			expectedErr: "error finding channel output for " +
				"matched order",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b1.SelfChanBalance = 100
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:        "invalid self chan balance",
			expectedErr: "server sent unexpected ending balance",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.BatchTX.TxOut[0].Value += 100
				b1.SelfChanBalance = 100
				return v.Verify(b, bestHeight)
			},
		},
		{
			name:        "happy path",
			expectedErr: "",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				return v.Verify(b, bestHeight)
			},
		},
		{
			name:         "happy path extend account support",
			batchVersion: ExtendAccountBatchVersion,
			expectedErr:  "",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				return v.Verify(b, bestHeight)
			},
		},
		{
			name:        "auction type mismatch",
			expectedErr: "did not match the same auction type",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				a.Details().AuctionType = BTCInboundLiquidity
				b1.Details().AuctionType = BTCOutboundLiquidity
				return v.Verify(b, bestHeight)
			},
		},
		{
			// TODO(positiveblue): make a more specific test for
			// btc outbound liquidity market.
			name:        "premiums are based on aucton type",
			expectedErr: "ending balance 934",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				a.Details().AuctionType = BTCOutboundLiquidity
				b1.Details().AuctionType = BTCOutboundLiquidity
				b2.Details().AuctionType = BTCOutboundLiquidity
				b.AccountDiffs[0].EndingBalance = 395_089
				b.BatchTX.TxOut[2].Value = 395_089

				// NOTE: the ask and bids would not be valid
				// orders for the outbound liquidity market
				// because they bigger than one unit but we are
				// testing that premiums are calculated
				// properly here.
				b.AccountDiffs[1].EndingBalance = 934
				return v.Verify(b, bestHeight)
			},
		},
	}

	// Run through all the test cases, creating a new, valid batch each
	// time so no state carries over from the last run.
	for _, tc := range testCases {

		// We'll create two accounts: A smaller one that has one ask for
		// 4 units that will be completely used up. Then a larger
		// account that has two bids that are both matched to the ask.
		// This account is large enough to be recreated, as it only
		// needs to pay for fees.
		bigAcct := &account.Account{
			TraderKey: &keychain.KeyDescriptor{
				PubKey: acctKeyBig,
			},
			Value:         500_000,
			Expiry:        144,
			State:         account.StateOpen,
			BatchKey:      startBatchKey,
			AuctioneerKey: startBatchKey,
		}
		smallAcct := &account.Account{
			TraderKey: &keychain.KeyDescriptor{
				PubKey: acctKeySmall,
			},
			Value:         400_840,
			Expiry:        144,
			State:         account.StateOpen,
			BatchKey:      startBatchKey,
			AuctioneerKey: startBatchKey,
		}
		ask := &Ask{
			Kit: newKitFromTemplate(Nonce{0x01}, &Kit{
				MultiSigKeyLocator: keychain.KeyLocator{
					Index: 0,
				},
				Units:            4,
				UnitsUnfulfilled: 4,
				MinUnitsMatch:    1,
				AcctKey:          acctIDSmall,
				FixedRate:        uint32(clearingPrice) / 2,
				LeaseDuration:    leaseDuration,
			}),
		}
		bid1 := &Bid{
			Kit: newKitFromTemplate(Nonce{0x02}, &Kit{
				MultiSigKeyLocator: keychain.KeyLocator{
					Index: 1,
				},
				Units:            2,
				UnitsUnfulfilled: 2,
				MinUnitsMatch:    1,
				AcctKey:          acctIDBig,
				FixedRate:        uint32(clearingPrice) * 2,
				// 1000 * (200_000 * 5000 / 1_000_000_000) = 1000 sats premium
				LeaseDuration: leaseDuration,
			}),
		}
		bid2 := &Bid{
			Kit: newKitFromTemplate(Nonce{0x03}, &Kit{
				MultiSigKeyLocator: keychain.KeyLocator{
					Index: 2,
				},
				Units:            8,
				UnitsUnfulfilled: 8,
				MinUnitsMatch:    1,
				AcctKey:          acctIDBig,
				FixedRate:        uint32(clearingPrice),
				// 2000 * (200_000 * 5000 / 1_000_000_000) = 1000 sats premium
				LeaseDuration: leaseDuration,
			}),
			SelfChanBalance: 100_000,
		}
		pkScript := scriptForAcct(t, bigAcct)
		// If account extension is supported we create the pkScript with
		// the expiry updated for one of the accounts (bigAcct).
		if tc.batchVersion.SupportsAccountExtension() {
			bigAcct.Expiry += uint32(extendAccountExpiry)
			pkScript = scriptForAcct(t, bigAcct)
			bigAcct.Expiry -= uint32(extendAccountExpiry)
		}
		batchTx := &wire.MsgTx{
			Version: 2,
			TxOut: []*wire.TxOut{
				// Channel output for channel between ask and
				// bid1.
				{
					Value: 200_000,
					PkScript: scriptForChan(
						t, walletKit,
						ask.MultiSigKeyLocator,
						bid1.MultiSigKeyLocator,
					),
				},
				// Channel output for channel between ask and
				// bid2.
				{
					Value: 300_000,
					PkScript: scriptForChan(
						t, walletKit,
						ask.MultiSigKeyLocator,
						bid2.MultiSigKeyLocator,
					),
				},
				// Recreated account output for large account.
				{
					// balance - bid1Premium - bid2Premium -
					// bid1ExecFee - bid2ExecFee - chainFees
					// - bid2SelfChanBalance
					// 500_000 - 1000 - 1000 -
					// 1_110 - 1_110 - 186 - 100_000
					Value:    395_594,
					PkScript: pkScript,
				},
			},
		}

		// Create a batch for us as if we were the trader for both
		// accounts and were matched against each other (not impossible
		// but unlikely to happen in the real system).
		accountDiffs := []*AccountDiff{
			{
				AccountKeyRaw: acctIDBig,
				AccountKey:    acctKeyBig,
				EndingState:   stateRecreated,
				OutpointIndex: 2,
				EndingBalance: 395_594,
			},
			{
				AccountKeyRaw: acctIDSmall,
				AccountKey:    acctKeySmall,
				EndingState:   stateExtendedOffchain,
				OutpointIndex: -1,
				EndingBalance: 434,
			},
		}

		// If account extension is supported bigAcct needs to reflect
		// the expiry changes in its AccountDiff.
		if tc.batchVersion.SupportsAccountExtension() {
			nExpiry := bigAcct.Expiry + uint32(extendAccountExpiry)
			accountDiffs[0].NewExpiry = nExpiry
		}

		matchedOrders := map[Nonce][]*MatchedOrder{
			ask.nonce: {
				{
					Order:       bid1,
					UnitsFilled: 2,
					MultiSigKey: deriveRawKey(
						t, walletKit,
						bid1.MultiSigKeyLocator,
					),
				},
				{
					Order:       bid2,
					UnitsFilled: 2,
					MultiSigKey: deriveRawKey(
						t, walletKit,
						bid2.MultiSigKeyLocator,
					),
				},
			},
			bid1.nonce: {{
				Order:       ask,
				UnitsFilled: 2,
				MultiSigKey: deriveRawKey(
					t, walletKit, ask.MultiSigKeyLocator,
				),
			}},
			bid2.nonce: {{
				Order:       ask,
				UnitsFilled: 2,
				MultiSigKey: deriveRawKey(
					t, walletKit, ask.MultiSigKeyLocator,
				),
			}},
		}
		batch := &Batch{
			ID:            batchID,
			Version:       tc.batchVersion,
			MatchedOrders: matchedOrders,
			AccountDiffs:  accountDiffs,
			ExecutionFee: terms.NewLinearFeeSchedule(
				execFeeBase, execFeeRate,
			),
			ClearingPrices: map[uint32]FixedRatePremium{
				1000: clearingPrice,
			},
			BatchTX:        batchTx,
			BatchTxFeeRate: chainfee.FeePerKwFloor,
			HeightHint:     bestHeight,
		}

		// Create the starting database state now.
		storeMock := newMockStore()
		verifier := &batchVerifier{
			wallet:        walletKit,
			orderStore:    storeMock,
			ourNodePubkey: nodePubkey,
			getAccount:    storeMock.getAccount,
			version:       tc.batchVersion,
		}
		storeMock.accounts = map[[33]byte]*account.Account{
			acctIDBig:   bigAcct,
			acctIDSmall: smallAcct,
		}
		storeMock.orders = map[Nonce]Order{
			ask.Nonce():  ask,
			bid1.Nonce(): bid1,
			bid2.Nonce(): bid2,
		}

		// Finally run the test case itself.
		t.Run(tc.name, func(t *testing.T) {
			err := tc.doVerify(verifier, ask, bid1, bid2, batch)

			if tc.expectedErr == "" {
				require.NoError(t, err, tc.name)
			} else {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.expectedErr, tc.name,
				)
			}
		})
	}
}

func newKitFromTemplate(nonce Nonce, tpl *Kit) Kit {
	kit := NewKit(nonce)
	kit.Version = tpl.Version
	kit.State = tpl.State
	kit.FixedRate = tpl.FixedRate
	kit.Amt = tpl.Amt
	kit.Units = tpl.Units
	kit.UnitsUnfulfilled = tpl.UnitsUnfulfilled
	kit.MinUnitsMatch = tpl.MinUnitsMatch
	kit.MultiSigKeyLocator = tpl.MultiSigKeyLocator
	kit.MaxBatchFeeRate = tpl.MaxBatchFeeRate
	kit.AcctKey = tpl.AcctKey
	kit.LeaseDuration = tpl.LeaseDuration
	return *kit
}

func scriptForChan(t *testing.T, walletKit *test.MockWalletKit, loc1,
	loc2 keychain.KeyLocator) []byte {

	key1 := deriveRawKey(t, walletKit, loc1)
	key2 := deriveRawKey(t, walletKit, loc2)
	_, out, err := input.GenFundingPkScript(key1[:], key2[:], 123)
	if err != nil {
		t.Fatalf("error generating funding script: %v", err)
	}
	return out.PkScript
}

func deriveRawKey(t *testing.T, walletKit *test.MockWalletKit,
	loc keychain.KeyLocator) [33]byte {

	key, err := walletKit.DeriveKey(context.Background(), &loc)
	if err != nil {
		t.Fatalf("error deriving key: %v", err)
	}
	var rawKey [33]byte
	copy(rawKey[:], key.PubKey.SerializeCompressed())
	return rawKey
}

func scriptForAcct(t *testing.T, acct *account.Account) []byte {
	script, err := acct.NextOutputScript()
	if err != nil {
		t.Errorf("error deriving next script: %v", err)
	}
	return script
}
