package order

import (
	"context"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/internal/test"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	_, startBatchKey = btcec.PrivKeyFromBytes(btcec.S256(), []byte{0x01})

	_, acctKeyBig = btcec.PrivKeyFromBytes(btcec.S256(), []byte{0x02})

	_, acctKeySmall = btcec.PrivKeyFromBytes(btcec.S256(), []byte{0x03})

	nodePubkey            = [33]byte{03, 77, 44, 55}
	execFeeBase           = btcutil.Amount(1_100)
	execFeeRate           = btcutil.Amount(50)
	clearingPrice         = FixedRatePremium(5000)
	stateRecreated        = poolrpc.AccountDiff_OUTPUT_RECREATED
	stateExtendedOffchain = poolrpc.AccountDiff_OUTPUT_DUST_EXTENDED_OFFCHAIN
)

func TestBatchVerifier(t *testing.T) {
	t.Parallel()

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
		name        string
		expectedErr string
		doVerify    func(BatchVerifier, *Ask, *Bid, *Bid, *Batch) error
	}{
		{
			name:        "version mismatch",
			expectedErr: ErrVersionMismatch.Error(),
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				return v.Verify(&Batch{Version: 999})
			},
		},
		{
			name:        "invalid order",
			expectedErr: "not found",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				arr := make([]*MatchedOrder, 0)
				b.MatchedOrders[Nonce{99, 99}] = arr
				return v.Verify(b)
			},
		},
		{
			name:        "invalid order type",
			expectedErr: "matched same type orders",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.MatchedOrders[a.nonce] = append(
					b.MatchedOrders[a.nonce],
					&MatchedOrder{
						Order: a,
					},
				)
				return v.Verify(b)
			},
		},
		{
			name:        "invalid node pubkey",
			expectedErr: "other order is an order from our node",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.MatchedOrders[a.nonce][0].NodeKey = nodePubkey
				return v.Verify(b)
			},
		},
		{
			name:        "ask max duration larger than bid",
			expectedErr: "duration not overlapping",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				a.LeaseDuration = 100
				return v.Verify(b)
			},
		},
		{
			name:        "ask fixed rate larger than bid",
			expectedErr: "ask price greater than bid price",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				a.FixedRate = 20000
				return v.Verify(b)
			},
		},
		{
			name:        "bid min duration larger than ask",
			expectedErr: "duration not overlapping",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				delete(b.MatchedOrders, a.nonce)
				b2.LeaseDuration = 5000
				return v.Verify(b)
			},
		},
		{
			name:        "bid fixed rate smaller than ask",
			expectedErr: "ask price greater than bid price",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				delete(b.MatchedOrders, a.nonce)
				a.FixedRate = b1.FixedRate + 1
				return v.Verify(b)
			},
		},
		{
			name:        "channel output not found, wrong value",
			expectedErr: "no channel output found in batch tx for",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.BatchTX.TxOut[0].Value = 123
				return v.Verify(b)
			},
		},
		{
			name:        "channel output not found, wrong script",
			expectedErr: "no channel output found in batch tx for",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.BatchTX.TxOut[0].PkScript = []byte{99, 88}
				return v.Verify(b)
			},
		},
		{
			name:        "invalid units filled",
			expectedErr: "invalid units to be filled for order",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.BatchTX.TxOut[0].Value = 900_000
				b.MatchedOrders[a.nonce][0].UnitsFilled = 9
				b.MatchedOrders[b1.nonce][0].UnitsFilled = 9
				return v.Verify(b)
			},
		},
		{
			name:        "invalid min units match",
			expectedErr: "units, but minimum is",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				a.MinUnitsMatch = b1.MinUnitsMatch * 100
				return v.Verify(b)
			},
		},
		{
			name:        "invalid funding TX fee rate",
			expectedErr: "server sent unexpected ending balance",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.BatchTxFeeRate *= 2
				return v.Verify(b)
			},
		},
		{
			name:        "invalid clearing price bid",
			expectedErr: "below clearing price",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				delete(b.MatchedOrders, a.nonce)
				b1.FixedRate = uint32(b.ClearingPrice) - 1
				return v.Verify(b)
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
						LeaseDuration:    1000,
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
				return v.Verify(b)
			},
		},
		{
			name:        "invalid execution fee rate",
			expectedErr: "server sent unexpected ending balance",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				b.ExecutionFee = terms.NewLinearFeeSchedule(1, 1)
				return v.Verify(b)
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
				return v.Verify(b)
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
				return v.Verify(b)
			},
		},
		{
			name:        "happy path",
			expectedErr: "",
			doVerify: func(v BatchVerifier, a *Ask, b1, b2 *Bid,
				b *Batch) error {

				return v.Verify(b)
			},
		},
	}

	// Run through all the test cases, creating a new, valid batch each
	// time so no state carries over from the last run.
	for i, tc := range testCases {
		tc := tc

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
			Value:         401_000,
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
				LeaseDuration:    1000,
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
				LeaseDuration: 1000,
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
				LeaseDuration: 1000,
			}),
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
					Value: 200_000,
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
					// 500_000 - 1000 - 1000 -
					// 1_110 - 1_110 - 186
					Value:    495_594,
					PkScript: scriptForAcct(t, bigAcct),
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
				EndingBalance: 495_594,
			},
			{
				AccountKeyRaw: acctIDSmall,
				AccountKey:    acctKeySmall,
				EndingState:   stateExtendedOffchain,
				OutpointIndex: -1,
				EndingBalance: 594,
			},
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
			Version:       VersionLeaseDurationBuckets,
			MatchedOrders: matchedOrders,
			AccountDiffs:  accountDiffs,
			ExecutionFee: terms.NewLinearFeeSchedule(
				execFeeBase, execFeeRate,
			),
			ClearingPrice:  clearingPrice,
			BatchTX:        batchTx,
			BatchTxFeeRate: chainfee.FeePerKwFloor,
		}

		// Create the starting database state now.
		storeMock := newMockStore()
		verifier := &batchVerifier{
			wallet:        walletKit,
			orderStore:    storeMock,
			ourNodePubkey: nodePubkey,
			getAccount:    storeMock.getAccount,
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
		i := i
		t.Run(tc.name, func(t *testing.T) {
			err := tc.doVerify(verifier, ask, bid1, bid2, batch)
			if (err == nil && tc.expectedErr != "") ||
				(err != nil && !strings.Contains(
					err.Error(), tc.expectedErr,
				)) {

				t.Fatalf("test #%v: unexpected error, got '%v' wanted "+
					"'%v'", i, err, tc.expectedErr)
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
